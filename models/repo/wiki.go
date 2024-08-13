// Copyright 2015 The Gogs Authors. All rights reserved.
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"fmt"
	"path/filepath"
	"strings"

	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/util"
)

// ErrWikiAlreadyExist represents a "WikiAlreadyExist" kind of error.
type ErrWikiAlreadyExist struct {
	Title string
}

// IsErrWikiAlreadyExist checks if an error is an ErrWikiAlreadyExist.
func IsErrWikiAlreadyExist(err error) bool {
	_, ok := err.(ErrWikiAlreadyExist)
	return ok
}

func (err ErrWikiAlreadyExist) Error() string {
	return fmt.Sprintf("wiki page already exists [title: %s]", err.Title)
}

func (err ErrWikiAlreadyExist) Unwrap() error {
	return util.ErrAlreadyExist
}

// WikiCloneLink returns clone URLs of repository wiki.
func (repo *Repository) WikiCloneLink() *CloneLink {
	return repo.cloneLink(true)
}

// WikiPath returns wiki data path by given user and repository name.
func WikiPath(userName, repoName string) string {
	return filepath.Join(user_model.UserPath(userName), strings.ToLower(repoName)+".wiki.git")
}

// WikiPath returns wiki data path for given repository.
func (repo *Repository) WikiPath() string {
	return WikiPath(repo.OwnerName, repo.Name)
}

// HasWiki returns true if repository has wiki.
func (repo *Repository) HasWiki() bool {
	isDir, err := util.IsDir(repo.WikiPath())
	if err != nil {
		log.Error("Unable to check if %s is a directory: %v", repo.WikiPath(), err)
	}
	return isDir
}
