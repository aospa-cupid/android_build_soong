// Copyright 2017 Google Inc. All rights reserved.
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

package cc

import (
	"android/soong/android"
	"android/soong/cc/config"
	"strings"

	"github.com/google/blueprint/proptools"
)

// LTO (link-time optimization) allows the compiler to optimize and generate
// code for the entire module at link time, rather than per-compilation
// unit. LTO is required for Clang CFI and other whole-program optimization
// techniques. LTO also allows cross-compilation unit optimizations that should
// result in faster and smaller code, at the expense of additional compilation
// time.
//
// To properly build a module with LTO, the module and all recursive static
// dependencies should be compiled with -flto which directs the compiler to emit
// bitcode rather than native object files. These bitcode files are then passed
// by the linker to the LLVM plugin for compilation at link time. Static
// dependencies not built as bitcode will still function correctly but cannot be
// optimized at link time and may not be compatible with features that require
// LTO, such as CFI.
//
// This file adds support to soong to automatically propogate LTO options to a
// new variant of all static dependencies for each module with LTO enabled.

type LTOProperties struct {
	// Lto must violate capitialization style for acronyms so that it can be
	// referred to in blueprint files as "lto"
	Lto struct {
		Never *bool `android:"arch_variant"`
		Full  *bool `android:"arch_variant"`
		Thin  *bool `android:"arch_variant"`
	} `android:"arch_variant"`

	// Dep properties indicate that this module needs to be built with LTO
	// since it is an object dependency of an LTO module.
	FullEnabled  bool `blueprint:"mutated"`
	ThinEnabled  bool `blueprint:"mutated"`
	NoLtoEnabled bool `blueprint:"mutated"`
	FullDep      bool `blueprint:"mutated"`
	ThinDep      bool `blueprint:"mutated"`
	NoLtoDep     bool `blueprint:"mutated"`

	// Use clang lld instead of gnu ld.
	Use_clang_lld *bool

	// Use -fwhole-program-vtables cflag.
	Whole_program_vtables *bool
}

type lto struct {
	Properties LTOProperties
}

func (lto *lto) props() []interface{} {
	return []interface{}{&lto.Properties}
}

func (lto *lto) begin(ctx BaseModuleContext) {
	if ctx.Config().IsEnvTrue("DISABLE_LTO") {
		lto.Properties.NoLtoEnabled = true
	}
}

func (lto *lto) useClangLld(ctx BaseModuleContext) bool {
	if lto.Properties.Use_clang_lld != nil {
		return Bool(lto.Properties.Use_clang_lld)
	}
	return true
}

func (lto *lto) flags(ctx BaseModuleContext, flags Flags) Flags {
	// TODO(b/131771163): Disable LTO when using explicit fuzzing configurations.
	// LTO breaks fuzzer builds.
	if inList("-fsanitize=fuzzer-no-link", flags.Local.CFlags) {
		return flags
	}

	// TODO(b/254713216): LTO doesn't work on riscv64 yet.
	if ctx.Arch().ArchType == android.Riscv64 {
		return flags
	}

	if lto.LTO(ctx) {
		var ltoCFlag string
		var ltoLdFlag string
		if lto.ThinLTO() {
			// TODO(b/129607781) sdclang does not currently support
			// the "-fsplit-lto-unit" option
			if flags.Sdclang && !strings.Contains(config.SDClangPath, "9.0") {
				ltoCFlag = "-flto=thin"
			} else {
				ltoCFlag = "-flto=thin -fsplit-lto-unit"
			}
		} else if lto.FullLTO() {
			ltoCFlag = "-flto"
		} else {
			ltoCFlag = "-flto=thin -fsplit-lto-unit"
			ltoLdFlag = "-Wl,--lto-O0"
		}

		flags.Local.CFlags = append(flags.Local.CFlags, ltoCFlag)
		flags.Local.AsFlags = append(flags.Local.AsFlags, ltoCFlag)
		flags.Local.LdFlags = append(flags.Local.LdFlags, ltoCFlag)
		flags.Local.LdFlags = append(flags.Local.LdFlags, ltoLdFlag)

		if Bool(lto.Properties.Whole_program_vtables) {
			flags.Local.CFlags = append(flags.Local.CFlags, "-fwhole-program-vtables")
		}

		if (lto.DefaultThinLTO(ctx) || lto.ThinLTO()) && ctx.Config().IsEnvTrue("USE_THINLTO_CACHE") && lto.useClangLld(ctx) {
			// Set appropriate ThinLTO cache policy
			cacheDirFormat := "-Wl,--thinlto-cache-dir="
			cacheDir := android.PathForOutput(ctx, "thinlto-cache").String()
			flags.Local.LdFlags = append(flags.Local.LdFlags, cacheDirFormat+cacheDir)

			// Limit the size of the ThinLTO cache to the lesser of 10% of available
			// disk space and 10GB.
			cachePolicyFormat := "-Wl,--thinlto-cache-policy="
			policy := "cache_size=10%:cache_size_bytes=10g"
			flags.Local.LdFlags = append(flags.Local.LdFlags, cachePolicyFormat+policy)
		}

		// If the module does not have a profile, be conservative and limit cross TU inline
		// limit to 40 LLVM IR instructions, to balance binary size increase and performance.
		if !ctx.isPgoCompile() && !ctx.isAfdoCompile() {
			flags.Local.LdFlags = append(flags.Local.LdFlags,
				"-Wl,-plugin-opt,-import-instr-limit=40")
			flags.Local.LdFlags = append(flags.Local.LdFlags,
				"-Wl,-mllvm,-inline-threshold=600")
			flags.Local.LdFlags = append(flags.Local.LdFlags,
				"-Wl,-mllvm,-inlinehint-threshold=750")
			flags.Local.LdFlags = append(flags.Local.LdFlags,
				"-Wl,-mllvm,-unroll-threshold=600")
		}
	}
	return flags
}

func (lto *lto) LTO(ctx BaseModuleContext) bool {
	return lto.ThinLTO() || lto.FullLTO() || lto.DefaultThinLTO(ctx)
}

func (lto *lto) DefaultThinLTO(ctx BaseModuleContext) bool {
	// LP32 has many subtle issues and less test coverage.
	lib32 := ctx.Arch().ArchType.Multilib == "lib32"
	// CFI enables full LTO.
	cfi := ctx.isCfi()
	// Performance and binary size are less important for host binaries and tests.
	host := ctx.Host()
	test := ctx.testBinary() || ctx.testLibrary()
	// FIXME: ThinLTO for VNDK produces different output.
	// b/169217596
	vndk := ctx.isVndk()
	return GlobalThinLTO(ctx) && !lto.Never() && !lib32 && !cfi && !host && !test && !vndk
}

func (lto *lto) FullLTO() bool {
	return lto != nil && (proptools.Bool(lto.Properties.Lto.Full) || lto.Properties.FullEnabled)
}

func (lto *lto) ThinLTO() bool {
	return lto != nil && (proptools.Bool(lto.Properties.Lto.Thin) || lto.Properties.ThinEnabled)
}

func (lto *lto) Never() bool {
	return lto != nil && (proptools.Bool(lto.Properties.Lto.Never) || lto.Properties.NoLtoEnabled)
}

func GlobalThinLTO(ctx android.BaseModuleContext) bool {
	return !ctx.Config().IsEnvFalse("GLOBAL_THINLTO")
}

// Propagate lto requirements down from binaries
func ltoDepsMutator(mctx android.TopDownMutatorContext) {
	globalThinLTO := GlobalThinLTO(mctx)

	if m, ok := mctx.Module().(*Module); ok {
		full := m.lto.FullLTO()
		thin := m.lto.ThinLTO()
		never := m.lto.Never()
		if full && thin {
			mctx.PropertyErrorf("LTO", "FullLTO and ThinLTO are mutually exclusive")
		}

		mctx.WalkDeps(func(dep android.Module, parent android.Module) bool {
			tag := mctx.OtherModuleDependencyTag(dep)
			libTag, isLibTag := tag.(libraryDependencyTag)

			// Do not recurse down non-static dependencies
			if isLibTag {
				if !libTag.static() {
					return false
				}
			} else {
				if tag != objDepTag && tag != reuseObjTag {
					return false
				}
			}

			if dep, ok := dep.(*Module); ok {
				if full && !dep.lto.FullLTO() {
					dep.lto.Properties.FullDep = true
				}
				if !globalThinLTO && thin && !dep.lto.ThinLTO() {
					dep.lto.Properties.ThinDep = true
				}
				if globalThinLTO && never && !dep.lto.Never() {
					dep.lto.Properties.NoLtoDep = true
				}
			}

			// Recursively walk static dependencies
			return true
		})
	}
}

// Create lto variants for modules that need them
func ltoMutator(mctx android.BottomUpMutatorContext) {
	globalThinLTO := GlobalThinLTO(mctx)

	if m, ok := mctx.Module().(*Module); ok && m.lto != nil {
		// Create variations for LTO types required as static
		// dependencies
		variationNames := []string{""}
		if m.lto.Properties.FullDep && !m.lto.FullLTO() {
			variationNames = append(variationNames, "lto-full")
		}
		if !globalThinLTO && m.lto.Properties.ThinDep && !m.lto.ThinLTO() {
			variationNames = append(variationNames, "lto-thin")
		}
		if globalThinLTO && m.lto.Properties.NoLtoDep && !m.lto.Never() {
			variationNames = append(variationNames, "lto-none")
		}

		// Use correct dependencies if LTO property is explicitly set
		// (mutually exclusive)
		if m.lto.FullLTO() {
			mctx.SetDependencyVariation("lto-full")
		}
		if !globalThinLTO && m.lto.ThinLTO() {
			mctx.SetDependencyVariation("lto-thin")
		}
		// Never must be the last, it overrides Thin or Full.
		if globalThinLTO && m.lto.Never() {
			mctx.SetDependencyVariation("lto-none")
		}

		if len(variationNames) > 1 {
			modules := mctx.CreateVariations(variationNames...)
			for i, name := range variationNames {
				variation := modules[i].(*Module)
				// Default module which will be
				// installed. Variation set above according to
				// explicit LTO properties
				if name == "" {
					continue
				}

				// LTO properties for dependencies
				if name == "lto-full" {
					variation.lto.Properties.FullEnabled = true
				}
				if name == "lto-thin" {
					variation.lto.Properties.ThinEnabled = true
				}
				if name == "lto-none" {
					variation.lto.Properties.NoLtoEnabled = true
				}
				variation.Properties.PreventInstall = true
				variation.Properties.HideFromMake = true
				variation.lto.Properties.FullDep = false
				variation.lto.Properties.ThinDep = false
				variation.lto.Properties.NoLtoDep = false
			}
		}
	}
}
