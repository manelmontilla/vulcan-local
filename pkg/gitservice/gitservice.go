/*
Copyright 2021 Adevinta
*/

package gitservice

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/adevinta/vulcan-agent/log"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/jesusfcr/gittp"
	"github.com/otiai10/copy"
	"github.com/phayes/freeport"
)

type GitService interface {
	AddGit(path string) (int, error)
	Shutdown()
}

type gitMapping struct {
	port   int
	server *http.Server
	tmpDir string
}

type gitService struct {
	log      log.Logger
	mappings map[string]*gitMapping
	wg       sync.WaitGroup
	mu       sync.Mutex
}

func New(l log.Logger) GitService {
	return &gitService{
		mappings: make(map[string]*gitMapping),
		log:      l,
	}
}

func (gs *gitService) AddGit(path string) (int, error) {
	// Prevent creating multiple gitservices for the same folder.
	gs.mu.Lock()
	defer gs.mu.Unlock()

	if mapping, ok := gs.mappings[path]; ok {
		return mapping.port, nil
	}
	tmpDir, err := gs.createTmpRepository(path)
	if err != nil {
		return 0, err
	}
	config := gittp.ServerConfig{
		Path:       tmpDir,
		Debug:      false,
		PreCreate:  gittp.UseGithubRepoNames,
		PreReceive: gittp.MasterOnly,
	}
	handle, err := gittp.NewGitServer(config)
	if err != nil {
		return 0, err
	}
	port, err := freeport.GetFreePort()
	if err != nil {
		return 0, err
	}

	r := gitMapping{
		port:   port,
		server: &http.Server{Addr: fmt.Sprintf("0.0.0.0:%d", port), Handler: handle},
		tmpDir: tmpDir,
	}
	gs.mappings[path] = &r
	gs.wg.Add(1)
	gs.log.Debugf("Starting git server path=%s port=%d", path, port)
	go func() {
		r.server.ListenAndServe()
		defer gs.wg.Done()
	}()
	return port, nil
}

func (gs *gitService) Shutdown() {
	for _, m := range gs.mappings {
		m.server.Shutdown(context.Background())
		os.RemoveAll(m.tmpDir)
	}
	gs.wg.Wait()
}

func (gs *gitService) createTmpRepository(path string) (string, error) {
	tmpRepositoryPath, err := os.MkdirTemp("", "")
	if err != nil {
		return "", err
	}

	var cmdOut, cmdErr bytes.Buffer
	ignore := map[string]bool{}
	cmd := exec.Command("git", "-C", path, "ls-files", "--exclude-standard", "-oi", "--directory")
	cmd.Stdout = &cmdOut
	cmd.Stderr = &cmdErr
	if err := cmd.Run(); err != nil {
		// The path is not part of a git repo... it's ok
		gs.log.Debugf("find .gitignored files error: %s.", cmdErr.String())
	} else {
		if cmdOut.Len() > 0 {
			for _, f := range strings.Split(cmdOut.String(), "\n") {
				f := strings.TrimSuffix(f, "/") // store directories without trailing slash
				f = filepath.Join(path, f)
				ignore[f] = true
			}
		}
	}

	err = copy.Copy(path, tmpRepositoryPath, copy.Options{Skip: func(srcinfo fs.FileInfo, src string, dest string) (bool, error) {
		_, ok := ignore[src]
		return ok || filepath.Base(src) == ".git", nil
	}})

	gs.log.Debugf("Copied %s to %s", path, tmpRepositoryPath)
	if err != nil {
		gs.log.Errorf("Error coping tmp file: %s", err)
		return "", err
	}
	r, _ := git.PlainInit(tmpRepositoryPath, false)
	w, err := r.Worktree()
	if err != nil {
		gs.log.Errorf("Error opening worktree: %s", err)
		return "", err
	}
	w.AddGlob(".")
	_, err = w.Commit("", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "vulcan",
			Email: "vulcan@adevinta.com",
		},
	})
	if err != nil {
		gs.log.Errorf("Error committing: %s", err)
		return "", err
	}
	return tmpRepositoryPath, nil
}
