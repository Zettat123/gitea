// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package wiki

import (
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/modules/git"
	api "code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/util"
	wiki_module "code.gitea.io/gitea/modules/wiki"
	"code.gitea.io/gitea/services/convert"
)

// ToWikiPageMetaData converts meta information to a WikiPageMetaData
func ToWikiPageMetaData(wikiName wiki_module.WebPath, lastCommit *git.Commit, repo *repo_model.Repository) *api.WikiPageMetaData {
	subURL := string(wikiName)
	_, title := wiki_module.WebPathToUserTitle(wikiName)
	return &api.WikiPageMetaData{
		Title:      title,
		HTMLURL:    util.URLJoin(repo.HTMLURL(), "wiki", subURL),
		SubURL:     subURL,
		LastCommit: convert.ToWikiCommit(lastCommit),
	}
}
