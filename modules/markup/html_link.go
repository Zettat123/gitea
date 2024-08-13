// Copyright 2024 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package markup

import (
	"path"
	"strings"

	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/util"
	wiki_module "code.gitea.io/gitea/modules/wiki"
)

func ResolveLink(ctx *RenderContext, link, userContentAnchorPrefix string) (result string, resolved bool) {
	isAnchorFragment := link != "" && link[0] == '#'
	if !isAnchorFragment && !IsFullURLString(link) {
		linkBase := ctx.Links.Base
		if ctx.IsWiki {
			if ext := path.Ext(link); ext == "" || ext == ".-" {
				linkBase = ctx.Links.WikiLink() // the link is for a wiki page
			} else if DetectMarkupTypeByFileName(link) != "" {
				linkBase = ctx.Links.WikiLink() // the link is renderable as a wiki page
			} else if hasMarkdownFile(ctx, link) {
				linkBase = ctx.Links.WikiLink()
			} else {
				linkBase = ctx.Links.WikiRawLink() // otherwise, use a raw link instead to view&download medias
			}
		} else if ctx.Links.BranchPath != "" || ctx.Links.TreePath != "" {
			// if there is no BranchPath, then the link will be something like "/owner/repo/src/{the-file-path}"
			// and then this link will be handled by the "legacy-ref" code and be redirected to the default branch like "/owner/repo/src/branch/main/{the-file-path}"
			linkBase = ctx.Links.SrcLink()
		}
		link, resolved = util.URLJoin(linkBase, link), true
	}
	if isAnchorFragment && userContentAnchorPrefix != "" {
		link, resolved = userContentAnchorPrefix+link[1:], true
	}
	return link, resolved
}

func hasMarkdownFile(ctx *RenderContext, link string) bool {
	if !ctx.IsWiki || ctx.Repo == nil || ctx.GitRepo == nil {
		return false
	}
	gitPath := wiki_module.WebPathToGitPath(wiki_module.WebPathFromRequest(link))
	_, err := ctx.GitRepo.LsFiles(gitPath)
	if err != nil {
		if strings.Contains(err.Error(), "Not a valid object name") {
			return false
		}
		log.Error("Wiki LsTree LsFiles, err: %v", err)
		return false
	}

	return true
}
