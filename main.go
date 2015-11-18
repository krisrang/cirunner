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
	app.Version = "0.0.1"
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
	go processRun(&wg, results, rspecSplit, buildname, buildid, "spec/reports", "spec", cmd...)

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

		go processRun(&wg, results, s, buildname, buildid, "features/reports", "features/reports/"+s.run, cmd...)
	}

	// Wait for runs to finish
	wg.Wait()

	// Results
	sort.Sort(RunResults(*results))
	success := true
	resultTbl := table.New(4)
	resultTbl.Add("Run", "Success", "Duration", "Comment")

	for _, r := range results.results {
		if !r.success {
			success = false
		}

		resultTbl.Add(r.run, strconv.FormatBool(r.success), formatDuration(r.duration), r.comment)
	}

	if !success {
		topic("Fail logs")
		for _, r := range results.results {
			if !r.success {
				msg(fmt.Sprintf("Run %v stdout:", r.run))
				fmt.Print(r.stdout.String())

				msg(fmt.Sprintf("Run %v stderr:", r.run))
				fmt.Print(r.stderr.String())
			}
		}
	}

	topic("Results")
	fmt.Print(resultTbl.String())

	if success {
		os.Exit(0)
	}
	os.Exit(1)
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

func processRun(wg *sync.WaitGroup, results *RunResults, s Split, buildname, buildid, reportSrc, reportDest string, cmd ...string) {
	start := time.Now()
	runcnt := fmt.Sprintf("%s-%s-%s", buildname, buildid, s.run)
	migratecnt := fmt.Sprintf("%s-prep", runcnt)
	dbcnt := fmt.Sprintf("%s-db", runcnt)
	rediscnt := fmt.Sprintf("%s-redis", runcnt)

	defer runCmd("docker", "rm", "-f", runcnt, migratecnt, dbcnt, rediscnt)
	runCmd("docker", "rm", "-f", runcnt, migratecnt, dbcnt, rediscnt)

	if err, stdout, stderr := runCmd("docker", "run", "-d", "--name", dbcnt, "-e", "MYSQL_ROOT_PASSWORD=jenkins", "mariadb:latest"); err != nil {
		setResult(wg, results, true, s.run, fmt.Sprintf("Starting DB failed: %v", err), start, stdout, stderr)
		return
	}
	if err, stdout, stderr := runCmd("docker", "run", "-d", "--name", rediscnt, "redis"); err != nil {
		setResult(wg, results, true, s.run, fmt.Sprintf("Starting redis failed: %v", err), start, stdout, stderr)
		return
	}

	// Wait for DB to boot
	time.Sleep(5 * time.Second)

	if err, stdout, stderr := runCmd("docker", "run", "--name", migratecnt,
		"-e", "RAILS_ENV=test",
		"--link", dbcnt+":db", "--link", rediscnt+":redis",
		buildname,
		"sh", "-c", "./bin/rake db:create db:schema:load db:migrate"); err != nil {
		setResult(wg, results, false, s.run, fmt.Sprintf("Setting up DB failed: %v", err), start, stdout, stderr)
		return
	}

	baseCmd := []string{
		"run",
		"--name",
		runcnt,
		"-e",
		"RAILS_ENV=test",
		"--link",
		dbcnt + ":db",
		"--link",
		rediscnt + ":redis",
		buildname,
	}

	err, stdout, stderr := runCmd("docker", append(baseCmd, cmd...)...)

	os.MkdirAll(reportDest, 0777)
	runCmd("docker", "cp", runcnt+":/app/"+reportSrc, reportDest)

	if err != nil {
		msg(fmt.Sprintf("Run %v failed, commiting as %v", s.run, runcnt))

		runCmd("docker", "commit", runcnt, runcnt)
		setResult(wg, results, false, s.run, "Run failed", start, stdout, stderr)
		return
	}

	msg(fmt.Sprintf("Run %v succeded", s.run))
	setResult(wg, results, true, s.run, "", start, stdout, stderr)
}

// Register run result
func setResult(wg *sync.WaitGroup, results *RunResults, success bool, run, comment string, start time.Time, stdout, stderr bytes.Buffer) {
	duration := time.Since(start)

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

	wg.Done()
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

func formatDuration(d time.Duration) string {
	return fmt.Sprintf("%0s", d-(d%time.Second))
}

func topic(name string) {
	fmt.Printf("===> %s\n", name)
}

func msg(name string) {
	fmt.Printf("     %s\n", name)
}