package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/krisrang/cirunner/Godeps/_workspace/src/github.com/codegangsta/cli"
	"github.com/krisrang/cirunner/Godeps/_workspace/src/github.com/pebblescape/pebblescape/pkg/random"
	"github.com/krisrang/cirunner/Godeps/_workspace/src/github.com/pebblescape/pebblescape/pkg/table"
	"github.com/krisrang/cirunner/cucumber"
)

var (
	verbose = false
	commit  = false
)

type Split struct {
	features []cucumber.FeatureFile
	run      string
}

type RunResult struct {
	success  bool
	run      string
	comment  string
	duration time.Duration
	stdout   bytes.Buffer
	stderr   bytes.Buffer
}

type RunResults struct {
	sync.RWMutex
	results []RunResult
}

func (a RunResults) Len() int           { return len(a.results) }
func (a RunResults) Swap(i, j int)      { a.results[i], a.results[j] = a.results[j], a.results[i] }
func (a RunResults) Less(i, j int) bool { return a.results[i].run < a.results[j].run }

func main() {
	app := cli.NewApp()
	app.Name = "cirunner"
	app.Version = "0.2.1"
	app.Action = run
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "path",
			Value: "./",
			Usage: "path to execute build in",
		},
		cli.StringFlag{
			Name:   "name",
			Value:  "",
			EnvVar: "JOB_NAME",
			Usage:  "name for this build",
		},
		cli.StringFlag{
			Name:   "id",
			Value:  "",
			EnvVar: "BUILD_ID",
			Usage:  "id for this build (defaults to 4 byte random hex)",
		},
		cli.StringSliceFlag{
			Name:   "tags",
			Usage:  "cucumber tags to filter features on",
			EnvVar: "CUCUMBER_TAGS",
		},
		cli.StringSliceFlag{
			Name:   "slowtags",
			Usage:  "cucumber tags to assign more step weight to",
			EnvVar: "CUCUMBER_SLOW_TAGS",
		},
		cli.BoolFlag{
			Name:  "verbose, vv",
			Usage: "verbose logging",
		},
		cli.BoolFlag{
			Name:  "commit",
			Usage: "commit run on failure",
		},
		cli.IntFlag{
			Name:  "maxruns",
			Value: 0,
			Usage: "maximum concurrent builds to run, defaults to number of CPUs",
		},
	}
	app.Run(os.Args)
}

func run(c *cli.Context) {
	path, _ := filepath.Abs(c.GlobalString("path"))
	buildname := c.GlobalString("name")
	buildid := c.GlobalString("id")
	tagArgs := c.GlobalStringSlice("tags")
	slowTags := c.GlobalStringSlice("slowtags")
	runs := c.GlobalInt("maxruns")
	verbose = c.GlobalBool("verbose")
	commit = c.GlobalBool("commit")

	if buildname == "" {
		log.Fatal("Must specify build name")
	}

	buildname = strings.ToLower(buildname)

	if buildid == "" {
		buildid = random.Hex(4)
	}

	if runs == 0 {
		runs = runtime.NumCPU()
	}

	topic(fmt.Sprintf("Starting build %s of %s", buildid, buildname))

	msg(fmt.Sprintf("Changing working directory to %s", path))
	if err := os.Chdir(path); err != nil {
		log.Fatal(err)
	}

	topic("Preparing config files and cleaning old reports")
	os.Rename("config/database.ci.yml", "config/database.yml")
	os.Rename("config/redis.ci.yml", "config/redis.yml")
	os.RemoveAll("spec/reports")
	os.RemoveAll("features/reports")

	topic("Building base image")
	if err, _, _ := runCmd("docker", "build", "-t", buildname, "."); err != nil {
		log.Fatal(err)
	}

	topic("Selecting features")
	tags := cucumber.ParseTags(tagArgs, slowTags)
	features, err := cucumber.Select(tags)
	if err != nil {
		log.Fatal(err)
	}

	if verbose {
		filesTbl := table.New(2)

		for _, f := range features {
			filesTbl.Add(f.Path, strconv.Itoa(f.Weight))
		}

		fmt.Print(filesTbl.String())
	}

	splits := splitFeatures(runs, features)
	runs = len(splits)

	topic("Starting database")
	dbcnt := fmt.Sprintf("%s-%s-db", buildname, buildid)
	runCmd("docker", "rm", "-f", "-v", dbcnt)
	if err, stdout, stderr := runCmd("docker", "run", "-d", "--name", dbcnt, "-e", "MYSQL_ROOT_PASSWORD=jenkins", "mariadb:latest"); err != nil {
		log.Fatal(fmt.Errorf("Starting DB failed: %v\n%s\n%s", err, stdout, stderr))
	}

	// Wait for DB to boot
	time.Sleep(10 * time.Second)

	// Cleanup all containers on interrupt
	go func() {
		sigchan := make(chan os.Signal, 10)
		signal.Notify(sigchan, os.Interrupt)
		<-sigchan
		log.Println("CI runner killed !")

		cmd := []string{
			"rm",
			"-f",
			"-v",
			dbcnt,
		}

		for _, s := range splits {
			cmd = append(cmd, fmt.Sprintf("%s-%s-%s", buildname, buildid, s.run))
			cmd = append(cmd, fmt.Sprintf("%s-%s-%s-redis", buildname, buildid, s.run))
		}

		cmd = append(cmd, fmt.Sprintf("%s-%s-%s", buildname, buildid, "rspec"))
		cmd = append(cmd, fmt.Sprintf("%s-%s-%s-redis", buildname, buildid, "rspec"))

		runCmd("docker", cmd...)

		os.Exit(1)
	}()

	// Run build
	topic(fmt.Sprintf("Running build in %v runs + rspec run", runs))
	results := &RunResults{
		results: make([]RunResult, 0),
	}

	wg := sync.WaitGroup{}
	wg.Add(runs + 1)

	// Run specs
	rspecSplit := Split{
		run: "rspec",
	}
	cmd := []string{
		"bundle", "exec", "rspec",
		"--format", "progress",
		"--format", "RspecJunitFormatter",
		"--out", "spec/reports/rspec.xml",
		"--color", "--no-drb",
	}
	go processRun(&wg, results, rspecSplit, buildname, buildid, "spec/reports", "spec", dbcnt, cmd...)

	// Run features
	for _, s := range splits {
		// Shuffle features
		for i := range s.features {
			j := rand.Intn(i + 1)
			s.features[i], s.features[j] = s.features[j], s.features[i]
		}

		cmd := []string{
			"bundle", "exec", "cucumber",
			"-r", "features",
			"--format", "progress",
			"--format", "junit",
			"--out", "features/reports",
			"--color", "--no-drb",
		}

		for _, t := range tagArgs {
			cmd = append(cmd, "--tags", t)
		}

		for _, f := range s.features {
			cmd = append(cmd, f.Path)
		}

		go processRun(&wg, results, s, buildname, buildid, "features/reports", "features/reports/"+s.run, dbcnt, cmd...)
	}

	// Wait for runs to finish
	wg.Wait()

	// Results
	sort.Sort(RunResults(*results))
	success := true
	resultTbl := table.New(4)
	resultTbl.Add("RUN", "SUCCESS", "DURATION", "")

	for _, r := range results.results {
		if !r.success {
			success = false
		}

		resultTbl.Add(r.run, strconv.FormatBool(r.success), formatDuration(r.duration), r.comment)
	}

	topic("Results")
	fmt.Print(resultTbl.String())

	if !success {
		os.Exit(cleanup(dbcnt, 1))
	}
	os.Exit(cleanup(dbcnt, 0))
}

func cleanup(dbcnt string, code int) int {
	defer runCmd("docker", "rm", "-f", "-v", dbcnt)

	return code
}

func splitFeatures(runs int, feat []cucumber.FeatureFile) map[int]Split {
	result := make(map[int]Split, 0)

	for i, f := range feat {
		id := (i + 1) % runs
		if id == 0 {
			id = runs
		}

		res, ok := result[id]
		if !ok {
			res = Split{
				features: make([]cucumber.FeatureFile, 0),
				run:      strconv.Itoa(id),
			}
		}

		res.features = append(res.features, f)
		result[id] = res
	}

	return result
}

func processRun(wg *sync.WaitGroup, results *RunResults, s Split, buildname, buildid, reportSrc, reportDest, dbcnt string, cmd ...string) {
	start := time.Now()
	runcnt := fmt.Sprintf("%s-%s-%s", buildname, buildid, s.run)
	dbname := strings.Replace(fmt.Sprintf("%s_%s_%s_test", buildname, buildid, s.run), "-", "_", -1)
	rediscnt := fmt.Sprintf("%s-redis", runcnt)

	defer func() {
		runCmd("docker", "rm", "-f", "-v", runcnt, rediscnt)
		wg.Done()
	}()

	runCmd("docker", "rm", "-f", "-v", runcnt, rediscnt)

	// Spin up redis
	if err, stdout, stderr := runCmd("docker", "run", "-d", "--name", rediscnt, "redis"); err != nil {
		setResult(results, true, s.run, fmt.Sprintf("Starting redis failed: %v", err), start, stdout, stderr)
		return
	}

	// Load up database schema and migrate
	if err, stdout, stderr := runCmd("docker", "run", "--rm",
		"-e", "RAILS_ENV=test",
		"-e", "DBNAME="+dbname,
		"--link", dbcnt+":db", "--link", rediscnt+":redis",
		buildname,
		"sh", "-c", "bundle exec rake db:create db:schema:load db:migrate"); err != nil {
		setResult(results, false, s.run, fmt.Sprintf("Migrating DB failed: %v", err), start, stdout, stderr)
		return
	}

	baseCmd := []string{
		"run",
		"--name",
		runcnt,
		"-e",
		"RAILS_ENV=test",
		"-e",
		"DBNAME=" + dbname,
		"--link",
		dbcnt + ":db",
		"--link",
		rediscnt + ":redis",
		buildname,
	}

	// TESTS! (=^ェ^=)
	err, stdout, stderr := runCmd("docker", append(baseCmd, cmd...)...)

	// Copy reports from container
	os.MkdirAll(reportDest, 0777)
	runCmd("docker", "cp", runcnt+":/app/"+reportSrc, reportDest)

	// Failed, commit the evidence!
	if err != nil {
		if commit {
			msg(fmt.Sprintf("Run %v failed, commiting as %v", s.run, runcnt))

			runCmd("docker", "commit", runcnt, runcnt)
		} else {
			msg(fmt.Sprintf("Run %v failed", s.run))
		}
		setResult(results, false, s.run, "Run failed", start, stdout, stderr)
		return
	}

	msg(fmt.Sprintf("Run %v succeded", s.run))
	setResult(results, true, s.run, "", start, stdout, stderr)
}

// Register run result
func setResult(results *RunResults, success bool, run, comment string, start time.Time, stdout, stderr bytes.Buffer) {
	duration := time.Since(start)

	if !success {
		fail(run, stdout, stderr)
	}

	results.Lock()
	defer results.Unlock()

	results.results = append(results.results, RunResult{
		success:  success,
		run:      run,
		comment:  comment,
		duration: duration,
		stdout:   stdout,
		stderr:   stderr,
	})
}

func runCmd(name string, args ...string) (error, bytes.Buffer, bytes.Buffer) {
	var outBuf bytes.Buffer
	var errBuf bytes.Buffer

	cmd := exec.Command(name, args...)

	if verbose {
		fmt.Printf("Running %v %v\n", name, args)
		cmd.Stdout = io.MultiWriter(bufio.NewWriter(&outBuf), os.Stdout)
		cmd.Stderr = io.MultiWriter(bufio.NewWriter(&errBuf), os.Stderr)
	} else {
		cmd.Stdout = bufio.NewWriter(&outBuf)
		cmd.Stderr = bufio.NewWriter(&errBuf)
	}

	return cmd.Run(), outBuf, errBuf
}

func pipeCmd(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)

	if verbose {
		fmt.Printf("Running %v %v\n", name, args)
	}

	return cmd.CombinedOutput()
}

func fmtOut(out []byte) {
	output := string(out)

	for _, line := range strings.Split(output, "\n") {
		msg(strings.Trim(line, " "))
	}
}

func fail(run string, stdout, stderr bytes.Buffer) {
	msg(fmt.Sprintf("Run %v stdout:", run))
	fmt.Print(stdout.String())

	msg(fmt.Sprintf("Run %v stderr:", run))
	fmt.Print(stderr.String())
}

func formatDuration(d time.Duration) string {
	return fmt.Sprintf("%0s", d-(d%time.Second))
}

func topic(name string) {
	fmt.Printf("===> %s\n", name)
}

func msg(name string) {
	fmt.Printf("     %s\n", name)
}
