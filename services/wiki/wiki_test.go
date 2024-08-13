// Copyright 2019 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package wiki

import (
	"testing"

	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unittest"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/gitrepo"
	wiki_module "code.gitea.io/gitea/modules/wiki"

	_ "code.gitea.io/gitea/models/actions"

	"github.com/stretchr/testify/assert"
)

func TestMain(m *testing.M) {
	unittest.MainTest(m)
}

func TestRepository_InitWiki(t *testing.T) {
	unittest.PrepareTestEnv(t)
	// repo1 already has a wiki
	repo1 := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	assert.NoError(t, InitWiki(git.DefaultContext, repo1))

	// repo2 does not already have a wiki
	repo2 := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 2})
	assert.NoError(t, InitWiki(git.DefaultContext, repo2))
	assert.True(t, repo2.HasWiki())
}

func TestRepository_AddWikiPage(t *testing.T) {
	assert.NoError(t, unittest.PrepareTestDatabase())
	const wikiContent = "This is the wiki content"
	const commitMsg = "Commit message"
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	doer := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	for _, userTitle := range []string{
		"Another page",
		"Here's a <tag> and a/slash",
	} {
		t.Run("test wiki exist: "+userTitle, func(t *testing.T) {
			webPath := wiki_module.UserTitleToWebPath("", userTitle)
			assert.NoError(t, AddWikiPage(git.DefaultContext, doer, repo, webPath, wikiContent, commitMsg))
			// Now need to show that the page has been added:
			gitRepo, err := gitrepo.OpenWikiRepository(git.DefaultContext, repo)
			if !assert.NoError(t, err) {
				return
			}
			defer gitRepo.Close()
			masterTree, err := gitRepo.GetTree(repo.DefaultWikiBranch)
			assert.NoError(t, err)
			gitPath := wiki_module.WebPathToGitPath(webPath)
			entry, err := masterTree.GetTreeEntryByPath(gitPath)
			assert.NoError(t, err)
			assert.EqualValues(t, gitPath, entry.Name(), "%s not added correctly", userTitle)
		})
	}

	t.Run("check wiki already exist", func(t *testing.T) {
		t.Parallel()
		// test for already-existing wiki name
		err := AddWikiPage(git.DefaultContext, doer, repo, "Home", wikiContent, commitMsg)
		assert.Error(t, err)
		assert.True(t, repo_model.IsErrWikiAlreadyExist(err))
	})

	t.Run("check wiki reserved name", func(t *testing.T) {
		t.Parallel()
		// test for reserved wiki name
		err := AddWikiPage(git.DefaultContext, doer, repo, "_edit", wikiContent, commitMsg)
		assert.Error(t, err)
		assert.True(t, wiki_module.IsErrWikiReservedName(err))
	})
}

func TestRepository_EditWikiPage(t *testing.T) {
	assert.NoError(t, unittest.PrepareTestDatabase())

	const newWikiContent = "This is the new content"
	const commitMsg = "Commit message"
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	doer := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	for _, newWikiName := range []string{
		"Home", // same name as before
		"New home",
		"New/name/with/slashes",
	} {
		webPath := wiki_module.UserTitleToWebPath("", newWikiName)
		unittest.PrepareTestEnv(t)
		assert.NoError(t, EditWikiPage(git.DefaultContext, doer, repo, "Home", webPath, newWikiContent, commitMsg))

		// Now need to show that the page has been added:
		gitRepo, err := gitrepo.OpenWikiRepository(git.DefaultContext, repo)
		assert.NoError(t, err)
		masterTree, err := gitRepo.GetTree(repo.DefaultWikiBranch)
		assert.NoError(t, err)
		gitPath := wiki_module.WebPathToGitPath(webPath)
		entry, err := masterTree.GetTreeEntryByPath(gitPath)
		assert.NoError(t, err)
		assert.EqualValues(t, gitPath, entry.Name(), "%s not edited correctly", newWikiName)

		if newWikiName != "Home" {
			_, err := masterTree.GetTreeEntryByPath("Home.md")
			assert.Error(t, err)
		}
		gitRepo.Close()
	}
}

func TestRepository_DeleteWikiPage(t *testing.T) {
	unittest.PrepareTestEnv(t)
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	doer := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	assert.NoError(t, DeleteWikiPage(git.DefaultContext, doer, repo, "Home"))

	// Now need to show that the page has been added:
	gitRepo, err := gitrepo.OpenWikiRepository(git.DefaultContext, repo)
	if !assert.NoError(t, err) {
		return
	}
	defer gitRepo.Close()
	masterTree, err := gitRepo.GetTree(repo.DefaultWikiBranch)
	assert.NoError(t, err)
	gitPath := wiki_module.WebPathToGitPath("Home")
	_, err = masterTree.GetTreeEntryByPath(gitPath)
	assert.Error(t, err)
}

func TestPrepareWikiFileName(t *testing.T) {
	unittest.PrepareTestEnv(t)
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	gitRepo, err := gitrepo.OpenWikiRepository(git.DefaultContext, repo)
	if !assert.NoError(t, err) {
		return
	}
	defer gitRepo.Close()

	tests := []struct {
		name      string
		arg       string
		existence bool
		wikiPath  string
		wantErr   bool
	}{{
		name:      "add suffix",
		arg:       "Home",
		existence: true,
		wikiPath:  "Home.md",
		wantErr:   false,
	}, {
		name:      "test special chars",
		arg:       "home of and & or wiki page!",
		existence: false,
		wikiPath:  "home-of-and-%26-or-wiki-page%21.md",
		wantErr:   false,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			webPath := wiki_module.UserTitleToWebPath("", tt.arg)
			existence, newWikiPath, err := prepareGitPath(gitRepo, repo.DefaultWikiBranch, webPath)
			if (err != nil) != tt.wantErr {
				assert.NoError(t, err)
				return
			}
			if existence != tt.existence {
				if existence {
					t.Errorf("expect to find no escaped file but we detect one")
				} else {
					t.Errorf("expect to find an escaped file but we could not detect one")
				}
			}
			assert.EqualValues(t, tt.wikiPath, newWikiPath)
		})
	}
}

func TestPrepareWikiFileName_FirstPage(t *testing.T) {
	unittest.PrepareTestEnv(t)

	// Now create a temporaryDirectory
	tmpDir := t.TempDir()

	err := git.InitRepository(git.DefaultContext, tmpDir, true, git.Sha1ObjectFormat.Name())
	assert.NoError(t, err)

	gitRepo, err := git.OpenRepository(git.DefaultContext, tmpDir)
	if !assert.NoError(t, err) {
		return
	}
	defer gitRepo.Close()

	existence, newWikiPath, err := prepareGitPath(gitRepo, "master", "Home")
	assert.False(t, existence)
	assert.NoError(t, err)
	assert.EqualValues(t, "Home.md", newWikiPath)
}
