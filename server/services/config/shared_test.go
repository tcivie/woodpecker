// Copyright 2024 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/server/forge/mocks"
	forge_types "go.woodpecker-ci.org/woodpecker/v3/server/forge/types"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/services/config"
)

const (
	testSharedRepo  = "shared-org/ci-shared"
	testSharedToken = "shared-token-xyz"
	testHeadSHA     = "deadbeefcafebabe"
)

type sharedFolder struct {
	path  string
	files []*forge_types.FileMeta
}

func setupSharedMock(t *testing.T, folders []sharedFolder) *mocks.MockForge {
	t.Helper()

	f := new(mocks.MockForge)

	f.On("Repo", mock.Anything, mock.Anything, mock.Anything, "shared-org", "ci-shared").Return(
		&model.Repo{Owner: "shared-org", Name: "ci-shared", FullName: "shared-org/ci-shared"},
		nil,
	)
	f.On("BranchHead", mock.Anything, mock.Anything, mock.Anything, "main").Return(
		&model.Commit{SHA: testHeadSHA},
		nil,
	)
	f.On("File", mock.Anything, mock.Anything, mock.Anything, mock.Anything, "groups.yaml").Return([]byte{}, &forge_types.ErrConfigNotFound{})

	for _, folder := range folders {
		f.On("Dir", mock.Anything, mock.Anything, mock.Anything, mock.Anything, folder.path).Return(folder.files, nil)
	}

	f.On("Dir", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, &forge_types.ErrConfigNotFound{})

	return f
}

func TestSharedFetch_LookupPrecedence(t *testing.T) {
	t.Parallel()

	dummy := []byte("pipeline: yes")

	testCases := []struct {
		name              string
		repoOwner         string
		repoName          string
		folders           []sharedFolder
		oldConfigData     []*forge_types.FileMeta
		expectedFileNames []string
	}{
		{
			name:      "same-name layered: namespace wins over repo-name and default",
			repoOwner: "acme",
			repoName:  "widget",
			folders: []sharedFolder{
				{path: "acme/widget", files: []*forge_types.FileMeta{{Name: "build.yml", Data: []byte("ns")}}},
				{path: "widget", files: []*forge_types.FileMeta{{Name: "build.yml", Data: []byte("repo-name")}}},
				{path: "default", files: []*forge_types.FileMeta{{Name: "build.yml", Data: []byte("default")}}},
			},
			expectedFileNames: []string{"build.yml"},
		},
		{
			name:      "different-name layered files stack from all layers",
			repoOwner: "acme",
			repoName:  "widget",
			folders: []sharedFolder{
				{path: "widget", files: []*forge_types.FileMeta{{Name: "build.yml", Data: dummy}}},
				{path: "default", files: []*forge_types.FileMeta{{Name: "deploy.yml", Data: dummy}}},
			},
			expectedFileNames: []string{"deploy.yml", "build.yml"},
		},
		{
			name:      "default fallback when nothing matches",
			repoOwner: "acme",
			repoName:  "widget",
			folders: []sharedFolder{
				{path: "default", files: []*forge_types.FileMeta{{Name: "build.yml", Data: dummy}}},
			},
			expectedFileNames: []string{"build.yml"},
		},
		{
			name:              "no match returns old config unchanged",
			repoOwner:         "acme",
			repoName:          "widget",
			folders:           nil,
			oldConfigData:     []*forge_types.FileMeta{{Name: ".woodpecker.yml", Data: dummy}},
			expectedFileNames: []string{".woodpecker.yml"},
		},
		{
			name:      "non-yaml files filtered out",
			repoOwner: "acme",
			repoName:  "widget",
			folders: []sharedFolder{
				{path: "acme/widget", files: []*forge_types.FileMeta{
					{Name: "README.md", Data: dummy},
					{Name: "build.yml", Data: dummy},
				}},
			},
			expectedFileNames: []string{"build.yml"},
		},
		{
			name:      "folder with only non-yaml falls through to default",
			repoOwner: "acme",
			repoName:  "widget",
			folders: []sharedFolder{
				{path: "acme/widget", files: []*forge_types.FileMeta{{Name: "README.md", Data: dummy}}},
				{path: "default", files: []*forge_types.FileMeta{{Name: "build.yaml", Data: dummy}}},
			},
			expectedFileNames: []string{"build.yaml"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			f := setupSharedMock(t, tc.folders)
			svc, err := config.NewShared(testSharedRepo, testSharedToken, time.Minute)
			require.NoError(t, err)

			repo := &model.Repo{Owner: tc.repoOwner, Name: tc.repoName, FullName: tc.repoOwner + "/" + tc.repoName}
			got, err := svc.Fetch(context.Background(), f, &model.User{Login: "triggering"}, repo, &model.Pipeline{}, tc.oldConfigData, false)
			require.NoError(t, err)

			names := make([]string, len(got))
			for i, fm := range got {
				names[i] = fm.Name
			}
			assert.ElementsMatch(t, tc.expectedFileNames, names)
		})
	}
}

func TestSharedFetch_FileLevelMerge(t *testing.T) {
	t.Parallel()

	repoData := []byte("from-repo")
	sharedData := []byte("from-shared")

	folders := []sharedFolder{
		{path: "acme/widget", files: []*forge_types.FileMeta{
			{Name: "build.yml", Data: sharedData},
			{Name: "deploy.yml", Data: sharedData},
		}},
	}

	oldConfig := []*forge_types.FileMeta{
		{Name: "build.yml", Data: repoData},
		{Name: "repo-only.yml", Data: repoData},
	}

	f := setupSharedMock(t, folders)
	svc, err := config.NewShared(testSharedRepo, testSharedToken, time.Minute)
	require.NoError(t, err)

	repo := &model.Repo{Owner: "acme", Name: "widget", FullName: "acme/widget"}
	got, err := svc.Fetch(context.Background(), f, &model.User{}, repo, &model.Pipeline{}, oldConfig, false)
	require.NoError(t, err)

	byName := map[string][]byte{}
	for _, fm := range got {
		byName[fm.Name] = fm.Data
	}

	assert.Equal(t, sharedData, byName["build.yml"], "shared file must override repo file with same name")
	assert.Equal(t, sharedData, byName["deploy.yml"])
	assert.Equal(t, repoData, byName["repo-only.yml"], "repo-unique file must survive merge")
	assert.Len(t, got, 3)
}

// On a restart/deploy, Woodpecker passes the persisted pipeline configs in as
// oldConfigData. Those entries have sanitized names ("build" — no extension),
// while a fresh folder fetch returns base names ("build.yml"). The merge must
// still recognize them as the same file or every restart doubles the workflows.
func TestSharedFetch_MergeMatchesExtensionStrippedOldConfig(t *testing.T) {
	t.Parallel()

	sharedData := []byte("from-shared")
	folders := []sharedFolder{
		{path: "default", files: []*forge_types.FileMeta{{Name: "build.yml", Data: sharedData}}},
	}
	oldConfig := []*forge_types.FileMeta{{Name: "build", Data: []byte("persisted")}}

	f := setupSharedMock(t, folders)
	svc, err := config.NewShared(testSharedRepo, testSharedToken, time.Minute)
	require.NoError(t, err)

	repo := &model.Repo{Owner: "acme", Name: "widget", FullName: "acme/widget"}
	got, err := svc.Fetch(context.Background(), f, &model.User{}, repo, &model.Pipeline{}, oldConfig, false)
	require.NoError(t, err)

	require.Len(t, got, 1, "sanitized old entry must collide with extensioned shared entry")
	assert.Equal(t, "build.yml", got[0].Name)
	assert.Equal(t, sharedData, got[0].Data)
}

func TestSharedFetch_Cache(t *testing.T) {
	t.Parallel()

	dummy := []byte("pipeline: yes")

	f := new(mocks.MockForge)
	f.On("Repo", mock.Anything, mock.Anything, mock.Anything, "shared-org", "ci-shared").Return(
		&model.Repo{Owner: "shared-org", Name: "ci-shared", FullName: "shared-org/ci-shared"},
		nil,
	)
	f.On("BranchHead", mock.Anything, mock.Anything, mock.Anything, "main").Return(
		&model.Commit{SHA: testHeadSHA},
		nil,
	)
	f.On("File", mock.Anything, mock.Anything, mock.Anything, mock.Anything, "groups.yaml").Return([]byte{}, &forge_types.ErrConfigNotFound{})
	dirCall := f.On("Dir", mock.Anything, mock.Anything, mock.Anything, mock.Anything, "acme/widget").Return(
		[]*forge_types.FileMeta{{Name: "build.yml", Data: dummy}},
		nil,
	)
	f.On("Dir", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, &forge_types.ErrConfigNotFound{})

	svc, err := config.NewShared(testSharedRepo, testSharedToken, time.Hour)
	require.NoError(t, err)

	repo := &model.Repo{Owner: "acme", Name: "widget", FullName: "acme/widget"}

	for i := 0; i < 5; i++ {
		_, err := svc.Fetch(context.Background(), f, &model.User{}, repo, &model.Pipeline{}, nil, false)
		require.NoError(t, err)
	}

	dirCallsForTarget := 0
	for _, c := range f.Calls {
		if c.Method == "Dir" && len(c.Arguments) >= 5 {
			if path, ok := c.Arguments[4].(string); ok && path == "acme/widget" {
				dirCallsForTarget++
			}
		}
	}
	assert.Equal(t, 1, dirCallsForTarget, "expected resolved folder to be fetched once under cache hit, got %d", dirCallsForTarget)
	_ = dirCall
}

func TestSharedFetch_CacheTTLExpiry(t *testing.T) {
	t.Parallel()

	dummy := []byte("pipeline: yes")

	f := new(mocks.MockForge)
	f.On("Repo", mock.Anything, mock.Anything, mock.Anything, "shared-org", "ci-shared").Return(
		&model.Repo{Owner: "shared-org", Name: "ci-shared", FullName: "shared-org/ci-shared"},
		nil,
	)
	f.On("BranchHead", mock.Anything, mock.Anything, mock.Anything, "main").Return(
		&model.Commit{SHA: testHeadSHA},
		nil,
	)
	f.On("File", mock.Anything, mock.Anything, mock.Anything, mock.Anything, "groups.yaml").Return([]byte{}, &forge_types.ErrConfigNotFound{})
	f.On("Dir", mock.Anything, mock.Anything, mock.Anything, mock.Anything, "acme/widget").Return(
		[]*forge_types.FileMeta{{Name: "build.yml", Data: dummy}},
		nil,
	)
	f.On("Dir", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, &forge_types.ErrConfigNotFound{})

	svc, err := config.NewShared(testSharedRepo, testSharedToken, time.Nanosecond)
	require.NoError(t, err)

	repo := &model.Repo{Owner: "acme", Name: "widget", FullName: "acme/widget"}

	_, err = svc.Fetch(context.Background(), f, &model.User{}, repo, &model.Pipeline{}, nil, false)
	require.NoError(t, err)

	time.Sleep(time.Millisecond)

	_, err = svc.Fetch(context.Background(), f, &model.User{}, repo, &model.Pipeline{}, nil, false)
	require.NoError(t, err)

	dirCallsForTarget := 0
	for _, c := range f.Calls {
		if c.Method == "Dir" && len(c.Arguments) >= 5 {
			if path, ok := c.Arguments[4].(string); ok && path == "acme/widget" {
				dirCallsForTarget++
			}
		}
	}
	assert.Equal(t, 2, dirCallsForTarget, "expired cache must refetch")
}

func TestSharedFetch_ForgeErrorPropagation(t *testing.T) {
	t.Parallel()

	t.Run("Repo lookup error", func(t *testing.T) {
		f := new(mocks.MockForge)
		f.On("Repo", mock.Anything, mock.Anything, mock.Anything, "shared-org", "ci-shared").Return(
			(*model.Repo)(nil),
			errors.New("not authorized"),
		)

		svc, err := config.NewShared(testSharedRepo, testSharedToken, time.Minute)
		require.NoError(t, err)

		old := []*forge_types.FileMeta{{Name: ".woodpecker.yml", Data: []byte("x")}}
		got, err := svc.Fetch(context.Background(), f, &model.User{}, &model.Repo{Owner: "acme", Name: "widget"}, &model.Pipeline{}, old, false)
		require.Error(t, err)
		assert.Equal(t, old, got)
	})

	t.Run("BranchHead error", func(t *testing.T) {
		f := new(mocks.MockForge)
		f.On("Repo", mock.Anything, mock.Anything, mock.Anything, "shared-org", "ci-shared").Return(
			&model.Repo{Owner: "shared-org", Name: "ci-shared", FullName: "shared-org/ci-shared"},
			nil,
		)
		f.On("BranchHead", mock.Anything, mock.Anything, mock.Anything, "main").Return(
			(*model.Commit)(nil),
			errors.New("branch missing"),
		)

		svc, err := config.NewShared(testSharedRepo, testSharedToken, time.Minute)
	f.On("File", mock.Anything, mock.Anything, mock.Anything, mock.Anything, "groups.yaml").Return([]byte{}, &forge_types.ErrConfigNotFound{})
		f.On("File", mock.Anything, mock.Anything, mock.Anything, mock.Anything, "groups.yaml").Return([]byte{}, &forge_types.ErrConfigNotFound{})
		require.NoError(t, err)

		old := []*forge_types.FileMeta{{Name: ".woodpecker.yml", Data: []byte("x")}}
		got, err := svc.Fetch(context.Background(), f, &model.User{}, &model.Repo{Owner: "acme", Name: "widget"}, &model.Pipeline{}, old, false)
		require.Error(t, err)
		assert.Equal(t, old, got)
	})

	t.Run("Dir unexpected error", func(t *testing.T) {
		f := new(mocks.MockForge)
		f.On("Repo", mock.Anything, mock.Anything, mock.Anything, "shared-org", "ci-shared").Return(
			&model.Repo{Owner: "shared-org", Name: "ci-shared", FullName: "shared-org/ci-shared"},
			nil,
		)
		f.On("BranchHead", mock.Anything, mock.Anything, mock.Anything, "main").Return(
			&model.Commit{SHA: testHeadSHA},
			nil,
		)
		f.On("File", mock.Anything, mock.Anything, mock.Anything, mock.Anything, "groups.yaml").Return([]byte{}, &forge_types.ErrConfigNotFound{})
		f.On("Dir", mock.Anything, mock.Anything, mock.Anything, mock.Anything, "acme/widget").Return(
			nil,
			fmt.Errorf("rate limited"),
		)
		f.On("Dir", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, &forge_types.ErrConfigNotFound{})

		svc, err := config.NewShared(testSharedRepo, testSharedToken, time.Minute)
		require.NoError(t, err)

		old := []*forge_types.FileMeta{{Name: ".woodpecker.yml", Data: []byte("x")}}
		got, err := svc.Fetch(context.Background(), f, &model.User{}, &model.Repo{Owner: "acme", Name: "widget"}, &model.Pipeline{}, old, false)
		require.Error(t, err)
		assert.Equal(t, old, got)
	})

	t.Run("Dir not-implemented falls through", func(t *testing.T) {
		dummy := []byte("pipeline: yes")
		f := new(mocks.MockForge)
		f.On("Repo", mock.Anything, mock.Anything, mock.Anything, "shared-org", "ci-shared").Return(
			&model.Repo{Owner: "shared-org", Name: "ci-shared", FullName: "shared-org/ci-shared"},
			nil,
		)
		f.On("BranchHead", mock.Anything, mock.Anything, mock.Anything, "main").Return(
			&model.Commit{SHA: testHeadSHA},
			nil,
		)
	f.On("File", mock.Anything, mock.Anything, mock.Anything, mock.Anything, "groups.yaml").Return([]byte{}, &forge_types.ErrConfigNotFound{})
		f.On("File", mock.Anything, mock.Anything, mock.Anything, mock.Anything, "groups.yaml").Return([]byte{}, &forge_types.ErrConfigNotFound{})
		f.On("Dir", mock.Anything, mock.Anything, mock.Anything, mock.Anything, "acme/widget").Return(nil, forge_types.ErrNotImplemented)
		f.On("Dir", mock.Anything, mock.Anything, mock.Anything, mock.Anything, "widget").Return(
			[]*forge_types.FileMeta{{Name: "build.yml", Data: dummy}},
			nil,
		)
		f.On("Dir", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, &forge_types.ErrConfigNotFound{})

		svc, err := config.NewShared(testSharedRepo, testSharedToken, time.Minute)
		require.NoError(t, err)

		got, err := svc.Fetch(context.Background(), f, &model.User{}, &model.Repo{Owner: "acme", Name: "widget"}, &model.Pipeline{}, nil, false)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "build.yml", got[0].Name)
	})
}

func TestSharedFetch_EmptySharedRepo(t *testing.T) {
	t.Parallel()

	f := new(mocks.MockForge)
	f.On("Repo", mock.Anything, mock.Anything, mock.Anything, "shared-org", "ci-shared").Return(
		&model.Repo{Owner: "shared-org", Name: "ci-shared", FullName: "shared-org/ci-shared"},
		nil,
	)
	f.On("BranchHead", mock.Anything, mock.Anything, mock.Anything, "main").Return(
		&model.Commit{SHA: testHeadSHA},
		nil,
	)
	f.On("File", mock.Anything, mock.Anything, mock.Anything, mock.Anything, "groups.yaml").Return([]byte{}, &forge_types.ErrConfigNotFound{})
	f.On("Dir", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, &forge_types.ErrConfigNotFound{})

	svc, err := config.NewShared(testSharedRepo, testSharedToken, time.Minute)
	require.NoError(t, err)

	old := []*forge_types.FileMeta{{Name: ".woodpecker.yml", Data: []byte("x")}}
	got, err := svc.Fetch(context.Background(), f, &model.User{}, &model.Repo{Owner: "acme", Name: "widget"}, &model.Pipeline{}, old, false)
	require.NoError(t, err)
	assert.Equal(t, old, got)
}

func TestNewShared_InvalidInput(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"", "no-slash", "/missing-owner", "missing-name/"} {
		_, err := config.NewShared(in, testSharedToken, time.Minute)
		assert.Error(t, err, "expected error for input %q", in)
	}
}
