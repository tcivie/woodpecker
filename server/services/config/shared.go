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

package config

import (
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"

	"go.woodpecker-ci.org/woodpecker/v3/server/forge"
	"go.woodpecker-ci.org/woodpecker/v3/server/forge/types"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

const (
	sharedBranch    = "main"
	groupsConfigKey = "groups.yaml"
)

// groupsConfig is the shape of `groups.yaml` at the root of the shared repo:
//
//	groups:
//	  backend:
//	    - acme/api
//	    - acme/worker
//	  frontend:
//	    - acme/web
//
// Repos can be listed by `name` or `owner/name`.
type groupsConfig struct {
	Groups map[string][]string `yaml:"groups"`
}

type cache[V any] interface {
	Get(key string) (V, bool)
	Set(key string, val V)
}

type ttlCache[V any] struct {
	ttl     time.Duration
	now     func() time.Time
	mu      sync.Mutex
	entries map[string]ttlCacheEntry[V]
}

type ttlCacheEntry[V any] struct {
	val       V
	expiresAt time.Time
}

func newTTLCache[V any](ttl time.Duration) *ttlCache[V] {
	return &ttlCache[V]{
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[string]ttlCacheEntry[V]),
	}
}

func (c *ttlCache[V]) Get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	e, ok := c.entries[key]
	if !ok {
		return zero, false
	}
	if c.now().After(e.expiresAt) {
		delete(c.entries, key)
		return zero, false
	}
	return e.val, true
}

func (c *ttlCache[V]) Set(key string, val V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = ttlCacheEntry[V]{
		val:       val,
		expiresAt: c.now().Add(c.ttl),
	}
}

type sharedFetcher struct {
	owner   string
	name    string
	token   string
	folders cache[[]*types.FileMeta]
	groups  cache[*groupsConfig]
}

func NewShared(sharedRepo, token string, ttl time.Duration) (Service, error) {
	owner, name, ok := strings.Cut(strings.TrimSpace(sharedRepo), "/")
	if !ok || owner == "" || name == "" {
		return nil, fmt.Errorf("shared config repo must be in 'owner/name' form, got %q", sharedRepo)
	}
	return &sharedFetcher{
		owner:   owner,
		name:    name,
		token:   token,
		folders: newTTLCache[[]*types.FileMeta](ttl),
		groups:  newTTLCache[*groupsConfig](ttl),
	}, nil
}

func (s *sharedFetcher) Fetch(ctx context.Context, f forge.Forge, _ *model.User, repo *model.Repo, _ *model.Pipeline, oldConfigData []*types.FileMeta, _ bool) ([]*types.FileMeta, error) {
	sharedUser := &model.User{
		Login:       s.owner,
		AccessToken: s.token,
		ForgeID:     repo.ForgeID,
	}

	sharedRepo, err := f.Repo(ctx, sharedUser, "", s.owner, s.name)
	if err != nil {
		return oldConfigData, fmt.Errorf("shared config: could not load shared repo %s/%s: %w", s.owner, s.name, err)
	}

	head, err := f.BranchHead(ctx, sharedUser, sharedRepo, sharedBranch)
	if err != nil {
		return oldConfigData, fmt.Errorf("shared config: could not resolve %s of shared repo: %w", sharedBranch, err)
	}
	sharedPipeline := &model.Pipeline{Commit: head.SHA}

	cfg := s.loadGroups(ctx, f, sharedUser, sharedRepo, sharedPipeline, head.SHA)
	repoGroups := matchingGroups(cfg, repo)
	if len(repoGroups) > 0 {
		log.Debug().Str("repo", repo.FullName).Strs("groups", repoGroups).Msg("shared config: matched groups")
	}

	layers := layerCandidates(repo, repoGroups)
	merged := append([]*types.FileMeta(nil), oldConfigData...)
	matchedAny := false
	for _, path := range layers {
		files, err := s.resolveFolder(ctx, f, sharedUser, sharedRepo, sharedPipeline, head.SHA, path)
		if err != nil {
			return oldConfigData, err
		}
		if files == nil {
			continue
		}
		merged = mergeFiles(merged, files)
		matchedAny = true
		log.Debug().
			Str("repo", repo.FullName).
			Str("shared_path", path).
			Int("files", len(files)).
			Msg("shared config: applied layer")
	}
	if !matchedAny {
		log.Trace().Str("repo", repo.FullName).Msg("shared config: no matching folder")
		return oldConfigData, nil
	}
	return merged, nil
}

// layerCandidates returns folder paths to merge in order. Later entries override
// earlier ones on filename collision.
func layerCandidates(repo *model.Repo, groups []string) []string {
	paths := []string{"default"}
	paths = append(paths, groups...)
	paths = append(paths, fmt.Sprintf("%s/%s", repo.Owner, repo.Name))
	paths = append(paths, repo.Name)
	return paths
}

// matchingGroups returns the names of groups the given repo belongs to. Order
// preserves the declaration order of groups.yaml.
func matchingGroups(cfg *groupsConfig, repo *model.Repo) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	for name, members := range cfg.Groups {
		for _, m := range members {
			if m == repo.Name || m == repo.FullName {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// loadGroups fetches and parses groups.yaml from the shared repo. Missing file
// or invalid YAML yields a nil config (no groups applied) — never an error.
func (s *sharedFetcher) loadGroups(ctx context.Context, f forge.Forge, user *model.User, sharedRepo *model.Repo, pipeline *model.Pipeline, headSHA string) *groupsConfig {
	if cached, ok := s.groups.Get(headSHA); ok {
		return cached
	}
	data, err := f.File(ctx, user, sharedRepo, pipeline, groupsConfigKey)
	if err != nil || len(data) == 0 {
		s.groups.Set(headSHA, nil)
		return nil
	}
	cfg := &groupsConfig{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		log.Warn().Err(err).Msg("shared config: groups.yaml parse failed; ignoring")
		s.groups.Set(headSHA, nil)
		return nil
	}
	s.groups.Set(headSHA, cfg)
	return cfg
}

func (s *sharedFetcher) resolveFolder(ctx context.Context, f forge.Forge, user *model.User, sharedRepo *model.Repo, pipeline *model.Pipeline, headSHA, path string) ([]*types.FileMeta, error) {
	key := headSHA + "\x00" + path
	if cached, ok := s.folders.Get(key); ok {
		return cached, nil
	}

	files, err := f.Dir(ctx, user, sharedRepo, pipeline, path)
	if err != nil {
		if errors.Is(err, &types.ErrConfigNotFound{}) || errors.Is(err, types.ErrNotImplemented) {
			s.folders.Set(key, nil)
			return nil, nil
		}
		return nil, fmt.Errorf("shared config: could not list %q in shared repo: %w", path, err)
	}
	files = filterPipelineFiles(files)
	if len(files) == 0 {
		s.folders.Set(key, nil)
		return nil, nil
	}
	for _, f := range files {
		f.Name = pathpkg.Base(f.Name)
	}
	s.folders.Set(key, files)
	return files, nil
}

// mergeFiles layers two file sets. b wins on filename collision; a-only files
// are preserved. Keys are compared by base name with .yaml/.yml stripped so
// that persisted configs (stored sanitized, no extension) collide with freshly
// fetched files (with extension).
func mergeFiles(a, b []*types.FileMeta) []*types.FileMeta {
	if len(b) == 0 {
		return a
	}
	overridden := make(map[string]struct{}, len(b))
	for _, f := range b {
		overridden[mergeKey(f.Name)] = struct{}{}
	}
	out := make([]*types.FileMeta, 0, len(a)+len(b))
	for _, f := range a {
		if _, replaced := overridden[mergeKey(f.Name)]; replaced {
			continue
		}
		out = append(out, f)
	}
	out = append(out, b...)
	return out
}

func mergeKey(name string) string {
	name = pathpkg.Base(name)
	for _, ext := range []string{".yaml", ".yml"} {
		if strings.HasSuffix(name, ext) {
			return name[:len(name)-len(ext)]
		}
	}
	return name
}
