// Copyright 2021 The gg Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//		 https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"gg-scm.io/pkg/git"
	"gg-scm.io/pkg/git/githash"
	"gg-scm.io/pkg/git/object"
	"gg-scm.io/pkg/git/packfile"
)

func TestPush(t *testing.T) {
	localGit, err := git.NewLocal(git.Options{})
	if err != nil {
		t.Skip("Can't find Git, skipping:", err)
	}

	for _, transport := range allTransportVariants(localGit.Exe()) {
		t.Run(transport.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			g := git.Custom(dir, localGit, localGit)
			if err := g.InitBare(ctx, "."); err != nil {
				t.Fatal(err)
			}
			const fname = "foo.txt"
			const fileContent = "Hello, World!\n"
			const commitMessage = "Initial import"
			const author object.User = "Octocat <octocat@example.com>"
			commitTime := time.Date(2020, time.January, 9, 14, 50, 0, 0, time.FixedZone("-0800", -8*60*60))
			blobObjectID, err := object.BlobSum(strings.NewReader(fileContent), int64(len(fileContent)))
			if err != nil {
				t.Fatal(err)
			}
			treeObject := object.Tree{
				{
					Name:     fname,
					Mode:     object.ModePlain,
					ObjectID: blobObjectID,
				},
			}
			commitObject := &object.Commit{
				Tree:       treeObject.SHA1(),
				Author:     author,
				AuthorTime: commitTime,
				Committer:  author,
				CommitTime: commitTime,
				Message:    commitMessage,
			}

			remote, err := NewRemote(transport.getURL(t, dir), nil)
			if err != nil {
				t.Fatal("NewRemote:", err)
			}
			stream, err := remote.StartPush(ctx)
			if err != nil {
				t.Fatal("remote.StartPush:", err)
			}
			targetRef := githash.BranchRef("main")
			err = stream.WriteCommands(&PushCommand{
				RefName: targetRef,
				New:     commitObject.SHA1(),
			})
			if err != nil {
				t.Error("PushStream.WriteCommands:", err)
			}
			pw := packfile.NewWriter(stream, 3)
			_, err = pw.WriteHeader(&packfile.Header{
				Type: packfile.Blob,
				Size: int64(len(fileContent)),
			})
			if err != nil {
				t.Error("WriteHeader:", err)
			}
			if _, err := io.WriteString(pw, fileContent); err != nil {
				t.Error("packfile.Writer.Write:", err)
			}
			treeObjectData := mustMarshalBinary(t, treeObject)
			_, err = pw.WriteHeader(&packfile.Header{
				Type: packfile.Tree,
				Size: int64(len(treeObjectData)),
			})
			if err != nil {
				t.Error("WriteHeader:", err)
			}
			if _, err := pw.Write(treeObjectData); err != nil {
				t.Error("packfile.Writer.Write:", err)
			}
			commitObjectData := mustMarshalBinary(t, commitObject)
			_, err = pw.WriteHeader(&packfile.Header{
				Type: packfile.Commit,
				Size: int64(len(commitObjectData)),
			})
			if err != nil {
				t.Error("WriteHeader:", err)
			}
			if _, err := pw.Write(commitObjectData); err != nil {
				t.Error("packfile.Writer.Write:", err)
			}
			if err := pw.Close(); err != nil {
				t.Error("packfile.Writer.Close:", err)
			}
			if err := stream.Close(); err != nil {
				t.Error("PushStream.Close:", err)
			}

			rev, err := g.ParseRev(ctx, targetRef.String())
			if err != nil {
				t.Fatal(err)
			}
			if rev.Commit != commitObject.SHA1() {
				t.Errorf("%v points to %v; want %v", targetRef, rev.Commit, commitObject)
			}
		})
	}
}
