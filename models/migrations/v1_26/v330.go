// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v1_26

import "xorm.io/xorm"

func AddReusableWorkflowCallFieldsToActionRunJob(x *xorm.Engine) error {
	type ActionRunJob struct {
		IsReusableCall       bool   `xorm:"index NOT NULL DEFAULT FALSE"`
		ReusableWorkflowUses string `xorm:"VARCHAR(255)"`
		ParentCallJobID      int64  `xorm:"index NOT NULL DEFAULT 0"`
		RootCallJobID        int64  `xorm:"index NOT NULL DEFAULT 0"`
		CallDepth            int    `xorm:"index NOT NULL DEFAULT 0"`
		CallEventPayload     string `xorm:"LONGTEXT"`
		CallSecretsInherit   bool   `xorm:"NOT NULL DEFAULT FALSE"`
		CallSecretNames      string `xorm:"LONGTEXT"`
	}

	_, err := x.SyncWithOptions(xorm.SyncOptions{IgnoreDropIndices: true}, new(ActionRunJob))
	return err
}
