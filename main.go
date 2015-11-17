package main

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/krisrang/cirunner/Godeps/_workspace/src/github.com/codegangsta/cli"
	"github.com/krisrang/cirunner/Godeps/_workspace/src/github.com/pebblescape/pebblescape/pkg/random"
	"github.com/krisrang/cirunner/Godeps/_workspace/src/github.com/pebblescape/pebblescape/pkg/table"
	"github.com/krisrang/cirunner/cucumber"
	"github.com/krisrang/cirunner/shell"
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
	verbose := c.GlobalBool("verbose")

	if buildname == "" {
		log.Fatal("Must specify build name")
	}

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
	if err := shell.Run("docker", "build", "-t", buildname, "."); err != nil {
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

	topic(fmt.Sprintf("Running build in %v runs + rspec run", runs))

	results := &RunResults{
		results: make([]RunResult, 0),
	}

	wg := sync.WaitGroup{}
	wg.Add(runs + 1)

	rspecSplit := Split{
		run: "rspec",
	}
	go runSpecs(&wg, results, rspecSplit, buildname, buildid)

	for _, s := range splits {
		go runSplit(&wg, results, s, buildname, buildid, tagArgs)
	}

	// Wait for runs to finish
	wg.Wait()

	topic("Results")

	sort.Sort(RunResults(*results))
	success := true
	resultTbl := table.New(4)
	resultTbl.Add("Run", "Success", "Duration", "Comment")

	for _, r := range results.results {
		if !r.success {
			success = false
		}

		resultTbl.Add(r.run, strconv.FormatBool(success), formatDuration(r.duration), r.comment)
	}

	fmt.Print(resultTbl.String())

	if success {
		os.Exit(0)
	}
	os.Exit(1)
}

func splitFeatures(runs int, feat []cucumber.FeatureFile) map[int]Split {
	result := make(map[int]Split, 0)

	// Shuffle features
	for i := range feat {
		j := rand.Intn(i + 1)
		feat[i], feat[j] = feat[j], feat[i]
	}

	sum := 0

	for _, f := range feat {
		sum += f.Weight
	}

	id := 1
	currentweight := 0
	weightbreak := sum / runs

	for _, f := range feat {
		if currentweight >= weightbreak {
			currentweight = 0
			id += 1
		} else {
			result[id] = Split{
				features: append(result[id].features, f),
				run:      strconv.Itoa(id),
			}

			currentweight += f.Weight
		}
	}

	return result
}

func runSplit(wg *sync.WaitGroup, results *RunResults, s Split, buildname, buildid string, tags []string) {
	cmd := []string{
		"bundle", "exec", "cucumber",
		"-r", "features",
		"--format", "progress",
		"--format", "junit",
		"--out", "features/reports",
		"--color", "--no-drb",
	}

	for _, t := range tags {
		cmd = append(cmd, "--tags", t)
	}

	for _, f := range s.features {
		cmd = append(cmd, f.Path)
	}

	processRun(wg, results, s, buildname, buildid, "features/reports", "features/reports/"+s.run, cmd...)
}

func runSpecs(wg *sync.WaitGroup, results *RunResults, s Split, buildname, buildid string) {
	cmd := []string{
		"bundle", "exec", "rspec",
		"--format", "progress",
		"--format", "RspecJunitFormatter",
		"--out", "spec/reports/rspec.xml",
		"--color", "--no-drb",
	}

	processRun(wg, results, s, buildname, buildid, "spec/reports", "spec", cmd...)
}

func processRun(wg *sync.WaitGroup, results *RunResults, s Split, buildname, buildid, reportSrc, reportDest string, cmd ...string) {
	start := time.Now()
	runcnt := fmt.Sprintf("%s-%s-%s", buildname, buildid, s.run)
	migratecnt := fmt.Sprintf("%s-prep", runcnt)
	dbcnt := fmt.Sprintf("%s-db", runcnt)
	rediscnt := fmt.Sprintf("%s-redis", runcnt)

	defer shell.Run("docker", "rm", "-f", runcnt, migratecnt, dbcnt, rediscnt)
	shell.Run("docker", "rm", "-f", runcnt, migratecnt, dbcnt, rediscnt)

	if err := shell.Run("docker", "run", "-d", "--name", dbcnt, "-e", "MYSQL_ROOT_PASSWORD=jenkins", "mariadb:latest"); err != nil {
		setResult(wg, results, true, s.run, fmt.Sprintf("Starting DB failed: %v", err), start)
		return
	}
	if err := shell.Run("docker", "run", "-d", "--name", rediscnt, "redis"); err != nil {
		setResult(wg, results, true, s.run, fmt.Sprintf("Starting redis failed: %v", err), start)
		return
	}

	// Wait for DB to boot
	time.Sleep(5 * time.Second)

	if err := shell.Run("docker", "run", "--name", migratecnt,
		"-e", "RAILS_ENV=test",
		"--link", dbcnt+":db", "--link", rediscnt+":redis", buildname,
		"sh", "-c", "./bin/rake db:create db:schema:load db:migrate"); err != nil {
		setResult(wg, results, false, s.run, fmt.Sprintf("Setting up DB failed: %v", err), start)
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

	err := shell.Run("docker", append(baseCmd, cmd...)...)

	os.MkdirAll(reportDest, 0777)
	shell.Run("docker", "cp", runcnt+":/app/"+reportSrc, reportDest)

	if err != nil {
		msg(fmt.Sprintf("Run %v failed, commiting as %v", s.run, runcnt))
		shell.Run("docker", "commit", runcnt, runcnt)
		setResult(wg, results, false, s.run, "Run failed", start)
		return
	}

	msg(fmt.Sprintf("Run %v succeded", s.run))
	setResult(wg, results, true, s.run, "", start)
}

// Register run result
func setResult(wg *sync.WaitGroup, results *RunResults, success bool, run, comment string, start time.Time) {
	duration := time.Since(start)

	results.Lock()
	defer results.Unlock()

	results.results = append(results.results, RunResult{
		success:  success,
		run:      run,
		comment:  comment,
		duration: duration,
	})

	wg.Done()
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
