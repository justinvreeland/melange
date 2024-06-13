// Copyright 2023 Chainguard, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package build

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"chainguard.dev/melange/pkg/cond"
	"chainguard.dev/melange/pkg/config"
	"chainguard.dev/melange/pkg/util"
	"github.com/chainguard-dev/clog"
	purl "github.com/package-url/packageurl-go"
	"gopkg.in/yaml.v3"
)

func (t *Test) Compile(ctx context.Context) error {
	cfg := t.Configuration

	// TODO: Make this parameter go away when we revisit subtitutions.
	flavor := "gnu"

	sm, err := NewSubstitutionMap(&cfg, t.Arch, flavor, nil)
	if err != nil {
		return err
	}

	c := &Compiled{
		PipelineDirs: t.PipelineDirs,
	}

	ignore := &Compiled{
		PipelineDirs: t.PipelineDirs,
	}

	// We want to evaluate this but not accumulate its deps.
	if err := ignore.CompilePipelines(ctx, sm, cfg.Pipeline); err != nil {
		return fmt.Errorf("compiling main pipelines: %w", err)
	}

	if err := c.CompilePipelines(ctx, sm, cfg.Test.Pipeline); err != nil {
		return fmt.Errorf("compiling main pipelines: %w", err)
	}

	for i, sp := range cfg.Subpackages {
		sm := sm.Subpackage(&sp)
		if sp.If != "" {
			sp.If, err = util.MutateAndQuoteStringFromMap(sm.Substitutions, sp.If)
			if err != nil {
				return fmt.Errorf("mutating subpackage if: %w", err)
			}
		}

		// We want to evaluate this but not accumulate its deps.
		if err := ignore.CompilePipelines(ctx, sm, sp.Pipeline); err != nil {
			return fmt.Errorf("compiling subpackage %q: %w", sp.Name, err)
		}

		test := &Compiled{
			PipelineDirs: t.PipelineDirs,
		}
		if err := c.CompilePipelines(ctx, sm, sp.Test.Pipeline); err != nil {
			return fmt.Errorf("compiling subpackage %q tests: %w", sp.Name, err)
		}

		te := &cfg.Subpackages[i].Test.Environment.Contents

		// Append the subpackage that we're testing to be installed.
		te.Packages = append(te.Packages, sp.Name)

		// Append anything this subpackage test needs.
		te.Packages = append(te.Packages, test.Needs...)
	}

	te := &t.Configuration.Test.Environment.Contents

	// Append the main test package to be installed unless explicitly specified by the command line.
	if t.Package != "" {
		te.Packages = append(te.Packages, t.Package)
	} else {
		te.Packages = append(te.Packages, t.Configuration.Package.Name)
	}

	// Append anything the main package test needs.
	te.Packages = append(te.Packages, c.Needs...)

	return nil
}

// Compile compiles all configuration, including tests, by loading any pipelines and substituting all variables.
func (b *Build) Compile(ctx context.Context) error {
	cfg := b.Configuration
	sm, err := NewSubstitutionMap(&cfg, b.Arch, b.BuildFlavor(), b.EnabledBuildOptions)
	if err != nil {
		return err
	}

	c := &Compiled{
		PipelineDirs: b.PipelineDirs,
	}

	if err := c.CompilePipelines(ctx, sm, cfg.Pipeline); err != nil {
		return fmt.Errorf("compiling main pipelines: %w", err)
	}

	for i, sp := range cfg.Subpackages {
		sm := sm.Subpackage(&sp)

		if sp.If != "" {
			sp.If, err = util.MutateAndQuoteStringFromMap(sm.Substitutions, sp.If)
			if err != nil {
				return fmt.Errorf("mutating subpackage if: %w", err)
			}
		}

		if err := c.CompilePipelines(ctx, sm, sp.Pipeline); err != nil {
			return fmt.Errorf("compiling subpackage %q: %w", sp.Name, err)
		}

		if sp.Test == nil {
			continue
		}

		tc := &Compiled{
			PipelineDirs: b.PipelineDirs,
		}
		if err := tc.CompilePipelines(ctx, sm, sp.Test.Pipeline); err != nil {
			return fmt.Errorf("compiling subpackage %q tests: %w", sp.Name, err)
		}

		te := &cfg.Subpackages[i].Test.Environment.Contents

		// Append the subpackage that we're testing to be installed.
		te.Packages = append(te.Packages, sp.Name)

		// Append anything this subpackage test needs.
		te.Packages = append(te.Packages, tc.Needs...)
	}

	ic := &b.Configuration.Environment.Contents
	ic.Packages = append(ic.Packages, c.Needs...)

	if cfg.Test != nil {
		tc := &Compiled{
			PipelineDirs: b.PipelineDirs,
		}

		if err := tc.CompilePipelines(ctx, sm, cfg.Test.Pipeline); err != nil {
			return fmt.Errorf("compiling main pipelines: %w", err)
		}

		te := &b.Configuration.Test.Environment.Contents
		te.Packages = append(te.Packages, tc.Needs...)

		// This can be overridden by the command line but in the context of a build, just use the main package.
		te.Packages = append(te.Packages, b.Configuration.Package.Name)
	}

	b.externalRefs = c.ExternalRefs

	return nil
}

type Compiled struct {
	PipelineDirs []string

	Needs        []string
	ExternalRefs []purl.PackageURL
}

func (c *Compiled) CompilePipelines(ctx context.Context, sm *SubstitutionMap, pipelines []config.Pipeline) error {
	for i := range pipelines {
		if err := c.compilePipeline(ctx, sm, &pipelines[i]); err != nil {
			return fmt.Errorf("compiling Pipeline[%d]: %w", i, err)
		}

		if err := c.gatherDeps(ctx, &pipelines[i]); err != nil {
			return fmt.Errorf("gathering deps for Pipeline[%d]: %w", i, err)
		}
	}

	return nil
}

func (c *Compiled) compilePipeline(ctx context.Context, sm *SubstitutionMap, pipeline *config.Pipeline) error {
	log := clog.FromContext(ctx)
	uses, with := pipeline.Uses, maps.Clone(pipeline.With)

	if uses != "" {
		var data []byte
		// Set this to fail up front in case there are no pipeline dirs specified
		// and we can't find them.
		err := fmt.Errorf("could not find 'uses' pipeline %q", uses)

		for _, pd := range c.PipelineDirs {
			log.Debugf("trying to load pipeline %q from %q", uses, pd)

			data, err = os.ReadFile(filepath.Join(pd, uses+".yaml"))
			if err == nil {
				log.Infof("Found pipeline %s", string(data))
				break
			}
		}
		if err != nil {
			log.Debugf("trying to load pipeline %q from embedded fs pipelines/%q.yaml", uses, uses)
			data, err = f.ReadFile("pipelines/" + uses + ".yaml")
			if err != nil {
				return fmt.Errorf("unable to load pipeline: %w", err)
			}
		}

		if err := yaml.Unmarshal(data, pipeline); err != nil {
			return fmt.Errorf("unable to parse pipeline %q: %w", uses, err)
		}
	}

	validated, err := validateWith(with, pipeline.Inputs)
	if err != nil {
		return fmt.Errorf("unable to validate with: %w", err)
	}

	mutated, err := sm.MutateWith(validated)
	if err != nil {
		return fmt.Errorf("mutating with: %w", err)
	}

	// allow input mutations on needs.packages
	if pipeline.Needs != nil {
		for i := range pipeline.Needs.Packages {
			pipeline.Needs.Packages[i], err = util.MutateStringFromMap(mutated, pipeline.Needs.Packages[i])
			if err != nil {
				return fmt.Errorf("mutating needs: %w", err)
			}
		}
	}

	if pipeline.WorkDir != "" {
		pipeline.WorkDir, err = util.MutateStringFromMap(mutated, pipeline.WorkDir)
		if err != nil {
			return fmt.Errorf("mutating workdir: %w", err)
		}
	}

	pipeline.Runs, err = util.MutateStringFromMap(mutated, pipeline.Runs)
	if err != nil {
		return fmt.Errorf("mutating runs: %w", err)
	}

	if pipeline.If != "" {
		pipeline.If, err = util.MutateAndQuoteStringFromMap(mutated, pipeline.If)
		if err != nil {
			return fmt.Errorf("mutating if: %w", err)
		}
	}

	// Compute external refs for this pipeline.
	externalRefs, err := computeExternalRefs(uses, mutated)
	if err != nil {
		return fmt.Errorf("computing external refs: %w", err)
	}

	c.ExternalRefs = append(c.ExternalRefs, externalRefs...)

	for i := range pipeline.Pipeline {
		p := &pipeline.Pipeline[i]
		p.With = util.RightJoinMap(mutated, p.With)

		if err := c.compilePipeline(ctx, sm, p); err != nil {
			return fmt.Errorf("compiling Pipeline[%d]: %w", i, err)
		}
	}

	// We only want to include "with"s that have non-default values.
	defaults := map[string]string{}
	for k, v := range pipeline.Inputs {
		defaults[k] = v.Default
	}
	cleaned := map[string]string{}
	for k := range with {
		nk := fmt.Sprintf("${{inputs.%s}}", k)

		nv := mutated[nk]
		if nv != defaults[k] {
			cleaned[k] = nv
		}
	}
	pipeline.With = cleaned

	// We don't care about the documented inputs.
	pipeline.Inputs = nil

	return nil
}

func identity(p *config.Pipeline) string {
	if p.Name != "" {
		return p.Name
	}
	if p.Uses != "" {
		return p.Uses
	}
	return "???"
}

func (c *Compiled) gatherDeps(ctx context.Context, pipeline *config.Pipeline) error {
	log := clog.FromContext(ctx)

	id := identity(pipeline)

	if pipeline.If != "" {
		if result, err := cond.Evaluate(pipeline.If); err != nil {
			return fmt.Errorf("evaluating conditional %q: %w", pipeline.If, err)
		} else if !result {
			return nil
		}
	}

	if pipeline.Needs != nil {
		for _, pkg := range pipeline.Needs.Packages {
			log.Infof("  adding package %q for pipeline %q", pkg, id)
		}
		c.Needs = append(c.Needs, pipeline.Needs.Packages...)

		pipeline.Needs = nil
	}

	for _, p := range pipeline.Pipeline {
		if err := c.gatherDeps(ctx, &p); err != nil {
			return err
		}
	}

	return nil
}
