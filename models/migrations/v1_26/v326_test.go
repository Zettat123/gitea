// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v1_26

import (
	"testing"

	"code.gitea.io/gitea/models/migrations/base"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/test"

	"github.com/stretchr/testify/require"
	"xorm.io/xorm"
)

func Test_FixCommitStatusTargetURLToUseRunAndJobID(t *testing.T) {
	defer test.MockVariableValue(&setting.AppSubURL, "")()

	type Repository struct {
		ID        int64 `xorm:"pk autoincr"`
		OwnerName string
		Name      string
	}

	type ActionRun struct {
		ID     int64 `xorm:"pk autoincr"`
		RepoID int64 `xorm:"index"`
		Index  int64
	}

	type ActionRunJob struct {
		ID    int64 `xorm:"pk autoincr"`
		RunID int64 `xorm:"index"`
	}

	type CommitStatus struct {
		ID        int64 `xorm:"pk autoincr"`
		RepoID    int64 `xorm:"index"`
		TargetURL string
	}

	type CommitStatusSummary struct {
		ID        int64  `xorm:"pk autoincr"`
		RepoID    int64  `xorm:"index"`
		SHA       string `xorm:"VARCHAR(64) NOT NULL"`
		State     string `xorm:"VARCHAR(7) NOT NULL"`
		TargetURL string
	}

	x, deferable := base.PrepareTestEnv(t, 0,
		new(Repository),
		new(ActionRun),
		new(ActionRunJob),
		new(CommitStatus),
		new(CommitStatusSummary),
	)
	defer deferable()

	repo := &Repository{ID: 1, OwnerName: "testuser", Name: "repo1"}
	_, err := x.Insert(repo)
	require.NoError(t, err)

	run := &ActionRun{ID: 106, RepoID: repo.ID, Index: 7}
	_, err = x.Insert(run)
	require.NoError(t, err)

	job0 := &ActionRunJob{ID: 530, RunID: run.ID}
	job1 := &ActionRunJob{ID: 531, RunID: run.ID}
	_, err = x.Insert(job0, job1)
	require.NoError(t, err)

	oldURL1 := "/testuser/repo1/actions/runs/7/jobs/0"
	newURL1 := "/testuser/repo1/actions/runs/106/jobs/530"
	oldURL2 := "/testuser/repo1/actions/runs/7/jobs/1"
	newURL2 := "/testuser/repo1/actions/runs/106/jobs/531"

	invalidWrongRepo := "/otheruser/badrepo/actions/runs/7/jobs/0"
	invalidNonexistentRun := "/testuser/repo1/actions/runs/10/jobs/0"
	invalidNonexistentJob := "/testuser/repo1/actions/runs/7/jobs/3"
	externalTargetURL := "https://ci.example.com/build/123"

	_, err = x.Insert(
		&CommitStatus{ID: 10, RepoID: repo.ID, TargetURL: oldURL1},
		&CommitStatus{ID: 11, RepoID: repo.ID, TargetURL: oldURL2},
		&CommitStatus{ID: 12, RepoID: repo.ID, TargetURL: invalidWrongRepo},
		&CommitStatus{ID: 13, RepoID: repo.ID, TargetURL: invalidNonexistentRun},
		&CommitStatus{ID: 14, RepoID: repo.ID, TargetURL: invalidNonexistentJob},
		&CommitStatus{ID: 15, RepoID: repo.ID, TargetURL: externalTargetURL},
	)
	require.NoError(t, err)

	_, err = x.Insert(
		&CommitStatusSummary{ID: 20, RepoID: repo.ID, SHA: "012345", State: "success", TargetURL: oldURL1},
		&CommitStatusSummary{ID: 21, RepoID: repo.ID, SHA: "678901", State: "success", TargetURL: externalTargetURL},
	)
	require.NoError(t, err)

	require.NoError(t, FixCommitStatusTargetURLToUseRunAndJobID(x))

	cases := []struct {
		table string
		id    int64
		want  string
	}{
		{table: "commit_status", id: 10, want: newURL1},
		{table: "commit_status", id: 11, want: newURL2},
		{table: "commit_status", id: 12, want: invalidWrongRepo},
		{table: "commit_status", id: 13, want: invalidNonexistentRun},
		{table: "commit_status", id: 14, want: invalidNonexistentJob},
		{table: "commit_status", id: 15, want: externalTargetURL},
		{table: "commit_status_summary", id: 20, want: newURL1},
		{table: "commit_status_summary", id: 21, want: externalTargetURL},
	}

	for _, tc := range cases {
		assertTargetURL(t, x, tc.table, tc.id, tc.want)
	}
}

func assertTargetURL(t *testing.T, x *xorm.Engine, table string, id int64, want string) {
	t.Helper()

	var row struct {
		TargetURL string
	}
	has, err := x.Table(table).Where("id=?", id).Cols("target_url").Get(&row)
	require.NoError(t, err)
	require.True(t, has)
	require.Equal(t, want, row.TargetURL)
}
