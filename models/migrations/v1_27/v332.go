// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v1_27

import (
	"xorm.io/xorm"
)

// AddReusableWorkflowFieldsToActionRunJob adds the columns required to represent reusable workflow caller jobs and their child jobs.
func AddReusableWorkflowFieldsToActionRunJob(x *xorm.Engine) error {
	type ActionRunJob struct {
		IsReusableCaller        bool   `xorm:"index NOT NULL DEFAULT FALSE"`
		ParentCallJobID         int64  `xorm:"index NOT NULL DEFAULT 0"`
		CallUses                string `xorm:"VARCHAR(512) NOT NULL DEFAULT ''"`
		CallSecrets             string `xorm:"LONGTEXT"`
		CallPayload             string `xorm:"LONGTEXT 'call_payload'"`
		ReusableWorkflowContent []byte `xorm:"LONGBLOB"`
	}

	_, err := x.SyncWithOptions(xorm.SyncOptions{IgnoreDropIndices: true}, new(ActionRunJob))
	return err
}
