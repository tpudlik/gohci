// Copyright 2016 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// gohci is the Go on Hardware CI.
//
// It is designed to test hardware based Go projects, e.g. testing the commits
// on Go project on a Rasberry Pi and updating the PR status on GitHub.
//
// It implements:
// - github webhook webserver that triggers on pushes and PRs
// - runs a Go build and a list of user supplied commands
// - posts the stdout to a Github gist and updates the commit's status
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/bugsnag/osext"
	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
)

type config struct {
	Port              int        // TCP port number for HTTP server.
	WebHookSecret     string     // https://developer.github.com/webhooks/
	Oauth2AccessToken string     // https://github.com/settings/tokens, check "repo:status" and "gist"
	Name              string     // Display name to use in the status report on Github.
	Checks            [][]string // Commands to run to test the repository. They are run one after the other from the repository's root.
}

func loadConfig(fileName string) (*config, error) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "gohci"
	}
	c := &config{
		Port:              8080,
		WebHookSecret:     "Create a secret and set it at github.com/user/repo/settings/hooks",
		Oauth2AccessToken: "Get one at https://github.com/settings/tokens",
		Name:              hostname,
		Checks:            [][]string{{"go", "test", "./..."}},
	}
	b, err := ioutil.ReadFile(fileName)
	if err != nil {
		b, err = json.MarshalIndent(c, "", "  ")
		if err != nil {
			return nil, err
		}
		if err = ioutil.WriteFile(fileName, b, 0600); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("wrote new %s", fileName)
	}
	if err = json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	d, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(b, d) {
		log.Printf("Updating %s in canonical format", fileName)
		if err := ioutil.WriteFile(fileName, d, 0600); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func normalizeUTF8(b []byte) []byte {
	if utf8.Valid(b) {
		return b
	}
	var out []byte
	for {
		r, size := utf8.DecodeRune(b)
		if r != utf8.RuneError {
			out = append(out, b[:size]...)
		}
		b = b[size:]
	}
	return out
}

func roundTime(t time.Duration) time.Duration {
	if t < time.Millisecond {
		// Precise at 1ns.
		return t
	}
	if t < time.Second {
		// Precise at 1µs.
		return (t + time.Microsecond/2) / time.Microsecond * time.Microsecond
	}
	// Round at 1ms
	return (t + time.Millisecond/2) / time.Millisecond * time.Millisecond
}

func run(cwd string, cmd ...string) (string, bool) {
	cmds := strings.Join(cmd, " ")
	log.Printf("- cwd=%s : %s", cwd, cmds)
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Dir = cwd
	start := time.Now()
	out, err := c.CombinedOutput()
	duration := time.Since(start)
	exit := 0
	if err != nil {
		exit = -1
		if len(out) == 0 {
			out = []byte("<failure>\n" + err.Error() + "\n")
		}
		if exiterr, ok := err.(*exec.ExitError); ok {
			if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				exit = status.ExitStatus()
			}
		}
	}
	return fmt.Sprintf("$ %s  (exit:%d in %s)\n%s", cmds, exit, roundTime(duration), normalizeUTF8(out)), err == nil
}

// file is an item in the gist.
type file struct {
	name, content string
	success       bool
	d             time.Duration
}

func metadata(commit, gopath string) string {
	return fmt.Sprintf(
		"Commit:  %s\nCPUs:    %d\nVersion: %s\nGOROOT:  %s\nGOPATH:  %s\nPATH:    %s",
		commit, runtime.NumCPU(), runtime.Version(), runtime.GOROOT(), gopath, os.Getenv("PATH"))
}

type item struct {
	content string
	ok      bool
}

func cloneOrFetch(repoPath, cloneURL string) (string, bool) {
	if _, err := os.Stat(repoPath); err == nil {
		return run(repoPath, "git", "fetch", "--prune", "--quiet")
	} else if !os.IsNotExist(err) {
		return "<failure>\n" + err.Error() + "\n", false
	}
	return run(path.Dir(repoPath), "git", "clone", "--quiet", cloneURL)
}

func fetch(repoPath string) (string, bool) {
	stdout, ok := run(repoPath, "git", "pull", "--prune", "--quiet")
	if !ok {
		// Give up and delete the repository. At worst "go get" will fetch
		// it below.
		if err := os.RemoveAll(repoPath); err != nil {
			// Deletion failed, that's a hard failure.
			return stdout + "<failure>\n" + err.Error() + "\n", false
		}
		return stdout + "<recovered failure>\nrm -rf " + repoPath + "\n", true
	}
	return stdout, ok
}

// syncParallel checkouts out one repository if missing, and syncs all the
// other git repositories found under the root directory concurrently.
//
// Since fetching is a remote operation with potentially low CPU and I/O,
// reduce the total latency by doing all the fetches concurrently.
//
// The goal is to make "go get -t -d" as fast as possible, as all repositories
// are already synced to HEAD.
func syncParallel(root, relRepo, cloneURL string, c chan<- item) {
	// relRepo is handled differently than the other.
	repoPath := filepath.Join(root, relRepo)
	// git clone / go get will have a race condition if the directory doesn't
	// exist.
	if err := os.MkdirAll(path.Dir(repoPath), 0700); err != nil && !os.IsExist(err) {
		c <- item{"<failure>\n" + err.Error() + "\n", false}
		return
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		stdout, ok := cloneOrFetch(repoPath, cloneURL)
		c <- item{stdout, ok}
	}()
	// Sync all the repositories concurrently.
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == repoPath {
			// repoPath is handled specifically above.
			return filepath.SkipDir
		}
		if fi.Name() == ".git" {
			path = filepath.Dir(path)
			wg.Add(1)
			go func(p string) {
				defer wg.Done()
				stdout, ok := fetch(p)
				c <- item{stdout, ok}
			}(path)
			return filepath.SkipDir
		}
		return nil
	})
	wg.Wait()
	if err != nil {
		c <- item{"<directory walking failure>\n" + err.Error() + "\n", false}
	}
}

// runChecks syncs then runs the checks and returns task's results.
//
// It aggressively concurrently fetches all repositories in `gopath` to
// accelerate the processing.
func runChecks(cmds [][]string, repoName string, useSSH bool, commit, gopath string, results chan<- file) bool {
	repoURL := "github.com/" + repoName
	src := filepath.Join(gopath, "src")
	c := make(chan item)
	cloneURL := "https://" + repoURL
	if useSSH {
		cloneURL = "git@github.com:" + repoName
	}
	start := time.Now()
	go func() {
		syncParallel(src, repoURL, cloneURL, c)
		close(c)
	}()
	setup := item{"", true}
	for i := range c {
		setup.content += i.content
		if !i.ok {
			setup.ok = false
		}
	}
	results <- file{"setup-1-sync", setup.content, setup.ok, time.Since(start)}
	if !setup.ok {
		return false
	}

	start = time.Now()
	repoPath := filepath.Join(src, repoURL)
	// go get will try to pull and will complain if the checkout is not on a
	// branch.
	stdout, ok := run(repoPath, "git", "checkout", "--quiet", "-B", "test", commit)
	// Reuse the object.
	setup.content = stdout
	if ok {
		stdout, ok = run(repoPath, "go", "get", "-v", "-d", "-t", "./...")
		setup.content += stdout
		if ok {
			// Precompilation has a dramatic effect on a Raspberry Pi.
			stdout, ok = run(repoPath, "go", "test", "-i", "./...")
			setup.content += stdout
		}
	}
	results <- file{"setup-2-get", setup.content, ok, time.Since(start)}
	setup.content = ""
	if ok {
		// Finally run the checks!
		for i, cmd := range cmds {
			start = time.Now()
			stdout, ok2 := run(repoPath, cmd...)
			results <- file{fmt.Sprintf("cmd%d", i+1), stdout, ok2, time.Since(start)}
			stdout = ""
			if !ok2 {
				// Still run the other tests.
				ok = false
			}
		}
	}
	return ok
}

type server struct {
	c      *config
	client *github.Client
	gopath string
	cmds   string
	mu     sync.Mutex     // Set when a check is running
	wg     sync.WaitGroup // Set for each pending task.
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("HTTP: %s %s", r.RemoteAddr, r.URL.Path)
	defer r.Body.Close()
	if r.Method != "POST" {
		http.Error(w, "Invalid method", http.StatusMethodNotAllowed)
		log.Printf("- invalid method")
		return
	}
	payload, err := github.ValidatePayload(r, []byte(s.c.WebHookSecret))
	if err != nil {
		http.Error(w, "Invalid secret", http.StatusUnauthorized)
		log.Printf("- invalid secret")
		return
	}
	if t := github.WebHookType(r); t != "ping" {
		event, err := github.ParseWebHook(t, payload)
		if err != nil {
			http.Error(w, "Invalid payload", http.StatusBadRequest)
			log.Printf("- invalid payload")
			return
		}
		// Process the rest asynchronously so the hook doesn't take too long.
		switch event := event.(type) {
		// TODO(maruel): For *github.CommitCommentEvent and
		// *github.IssueCommentEvent, when the comment is 'run tests' from a
		// collaborator, run the tests.
		case *github.PullRequestEvent:
			// s.client.Repositories.IsCollaborator() requires *write* access to the
			// repository, which we really do not want here.
			log.Printf("- PR %s #%d %s %s", *event.Repo.FullName, *event.PullRequest.ID, *event.Sender.Login, *event.Action)
			if *event.Action != "opened" && *event.Action != "synchronized" {
				log.Printf("- ignoring action %q for PR from %q", *event.Action, *event.Sender.Login)
			} else if *event.Repo.FullName != *event.PullRequest.Head.Repo.FullName {
				log.Printf("- ignoring PR from forked repo %q", *event.PullRequest.Head.Repo.FullName)
			} else {
				s.runCheckAsync(*event.Repo.FullName, *event.PullRequest.Head.SHA, *event.Repo.Private)
			}
		case *github.PushEvent:
			if event.HeadCommit == nil {
				log.Printf("- Push %s %s <deleted>", *event.Repo.FullName, *event.Ref)
			} else {
				log.Printf("- Push %s %s %s", *event.Repo.FullName, *event.Ref, *event.HeadCommit.ID)
				if !strings.HasPrefix(*event.Ref, "refs/heads/master") {
					log.Printf("- ignoring branch %q for push", *event.Ref)
				} else {
					s.runCheckAsync(*event.Repo.FullName, *event.HeadCommit.ID, *event.Repo.Private)
				}
			}
		default:
			log.Printf("- ignoring hook type %s", reflect.TypeOf(event).Elem().Name())
		}
	}
	io.WriteString(w, "{}")
}

// Immediately add the status that the test run is pending and add the run in
// the queue. Ensures that the service doesn't restart until the task is done.
func (s *server) runCheckAsync(repo, commit string, useSSH bool) {
	s.wg.Add(1)
	defer s.wg.Done()
	log.Printf("- Enqueuing test for %s at %s", repo, commit)
	// https://developer.github.com/v3/repos/statuses/#create-a-status
	status := &github.RepoStatus{
		State:       github.String("failure"),
		Description: github.String(fmt.Sprintf("Tests pending (0/%d)", len(s.c.Checks)+2)),
		Context:     &s.c.Name,
	}
	parts := strings.SplitN(repo, "/", 2)
	if _, _, err := s.client.Repositories.CreateStatus(parts[0], parts[1], commit, status); err != nil {
		// Don't bother running the tests.
		log.Printf("- Failed to create status: %v", err)
		return
	}
	// Enqueue and run.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runCheckSync(repo, commit, useSSH, status)
	}()
}

func (s *server) runCheckSync(repo, commit string, useSSH bool, status *github.RepoStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	log.Printf("- Running test for %s at %s", repo, commit)
	total := len(s.c.Checks) + 2
	suffix := fmt.Sprintf(" (0/%d)", total)
	// https://developer.github.com/v3/gists/#create-a-gist
	// It is still accessible via the URL without authentication.
	gistDesc := fmt.Sprintf("%s for https://github.com/%s/commit/%s", s.c.Name, repo, commit[:12])
	gist := &github.Gist{
		Description: github.String(gistDesc + suffix),
		Public:      github.Bool(false),
		Files: map[github.GistFilename]github.GistFile{
			"setup-0-metadata": github.GistFile{Content: github.String(metadata(commit, s.gopath) + "\nCommands to be run:\n" + s.cmds)},
		},
	}
	gist, _, err := s.client.Gists.Create(gist)
	if err != nil {
		// Don't bother running the tests.
		log.Printf("- Failed to create gist: %v", err)
		return
	}
	log.Printf("- Gist at %s", *gist.HTMLURL)

	statusDesc := "Running tests"
	status.TargetURL = gist.HTMLURL
	status.Description = github.String(statusDesc + suffix)
	parts := strings.SplitN(repo, "/", 2)
	if _, _, err = s.client.Repositories.CreateStatus(parts[0], parts[1], commit, status); err != nil {
		log.Printf("- Failed to update status: %v", err)
		return
	}

	start := time.Now()
	results := make(chan file)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		runChecks(s.c.Checks, repo, useSSH, commit, s.gopath, results)
		close(results)
	}()
	i := 1
	failed := false
	for r := range results {
		// https://developer.github.com/v3/gists/#edit-a-gist
		if len(r.content) == 0 {
			r.content = "<missing>"
		}
		if !r.success {
			r.name += " (failed)"
			failed = true
		}
		r.name += " in " + roundTime(r.d).String()
		gist.Files = map[github.GistFilename]github.GistFile{github.GistFilename(r.name): github.GistFile{Content: &r.content}}
		suffix = ""
		if i != total {
			suffix = fmt.Sprintf(" (%d/%d)", i, total)
		} else {
			statusDesc = "Ran tests"
			if !failed {
				suffix += " (success!)"
				status.State = github.String("success")
			}
		}
		if failed {
			suffix += " (failed)"
		}
		suffix += " in " + roundTime(time.Since(start)).String()
		gist.Description = github.String(gistDesc + suffix)
		if gist, _, err = s.client.Gists.Edit(*gist.ID, gist); err != nil {
			// Just move on.
			log.Printf("- failed to update gist: %v", err)
		}
		gist.Files = nil
		status.Description = github.String(statusDesc + suffix)
		if _, _, err = s.client.Repositories.CreateStatus(parts[0], parts[1], commit, status); err != nil {
			// Just move on.
			log.Printf("- failed to update status: %v", err)
		}
		i++
	}
	// TODO(maruel): If running on a push to refs/heads/master and it failed,
	// call s.client.Issues.Create().
	log.Printf("- testing done: https://github.com/%s/commit/%s", repo, commit[:12])
}

func mainImpl() error {
	test := flag.String("test", "", "runs a simulation locally, specify the git repository name (not URL) to test, e.g. 'maruel/gohci'")
	commit := flag.String("commit", "", "commit SHA1 to test and update; will only update status on github if not 'HEAD'")
	useSSH := flag.Bool("usessh", false, "use SSH to fetch the repository instead of HTTPS; only necessary when testing")
	flag.Parse()
	log.SetFlags(0)
	if *test == "" {
		if *commit != "" {
			return errors.New("-commit doesn't make sense without -test")
		}
		if *useSSH {
			return errors.New("-usessh doesn't make sense without -test")
		}
	} else if *commit == "" {
		*commit = "HEAD"
	}
	fileName := "gohci.json"
	c, err := loadConfig(fileName)
	if err != nil {
		return err
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	gopath := filepath.Join(wd, "go")
	// GOPATH may not be set especially when running from systemd, so use the
	// local GOPATH to install gt. This is safer as this doesn't modify the host
	// environment.
	os.Setenv("GOPATH", gopath)
	os.Setenv("PATH", filepath.Join(gopath, "bin")+":"+os.Getenv("PATH"))
	hasTest := false
	for _, cmd := range c.Checks {
		if len(cmd) >= 2 && cmd[0] == "go" && cmd[1] == "test" {
			hasTest = true
			break
		}
	}
	if hasTest {
		stdout, useGT := run(wd, "go", "get", "rsc.io/gt")
		if useGT {
			log.Print("Using gt")
			os.Setenv("CACHE", gopath)
			for i, cmd := range c.Checks {
				if len(cmd) >= 2 && cmd[0] == "go" && cmd[1] == "test" {
					cmd[1] = "gt"
					c.Checks[i] = cmd[1:]
				}
			}
		} else {
			log.Print(stdout)
		}
	}
	cmds := ""
	for i, cmd := range c.Checks {
		if i != 0 {
			cmds += "\n"
		}
		cmds += "  " + strings.Join(cmd, " ")
	}
	tc := oauth2.NewClient(oauth2.NoContext, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: c.Oauth2AccessToken}))
	s := server{c: c, client: github.NewClient(tc), gopath: gopath, cmds: cmds}
	if len(*test) != 0 {
		if *commit == "HEAD" {
			// Only run locally.
			results := make(chan file)
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range results {
					if !i.success {
						i.name += " (failed)"
					}
					fmt.Printf("--- %s\n%s", i.name, i.content)
				}
			}()
			fmt.Printf("--- setup-0-metadata\n%s", metadata(*commit, gopath))
			success := runChecks(c.Checks, *test, *useSSH, *commit, gopath, results)
			close(results)
			wg.Wait()
			_, err := fmt.Printf("\nSuccess: %t\n", success)
			return err
		}
		s.runCheckAsync(*test, *commit, *useSSH)
		s.wg.Wait()
		// TODO(maruel): Return any error that occured.
		return nil
	}
	http.Handle("/", &s)
	thisFile, err := osext.Executable()
	if err != nil {
		return err
	}
	log.Printf("Running in: %s", wd)
	log.Printf("Executable: %s", thisFile)
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", c.Port))
	if err != nil {
		return err
	}
	a := ln.Addr().String()
	ln.Close()
	log.Printf("Listening on: %s", a)
	go http.ListenAndServe(a, nil)
	err = watchFiles(thisFile, fileName)
	// Ensures no task is running.
	s.wg.Wait()
	return err
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "gohci: %s.\n", err)
		os.Exit(1)
	}
}
