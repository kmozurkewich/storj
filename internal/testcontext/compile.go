// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package testcontext

import (
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"testing"
)

// CLibMath is the standard C math library (see `man math.h`).
var CLibMath = Include{Standard: true, Library: "m"}

// CompileCOptions stores options for compiling C source to an executable.
type CompileCOptions struct {
	Dest     string
	Sources  []string
	Includes []Include
	NoWarn   bool
}

// Compile compiles the specified package and returns the executable name.
func (ctx *Context) Compile(pkg string) string {
	ctx.test.Helper()

	exe := ctx.File("build", path.Base(pkg)+".exe")

	var cmd *exec.Cmd
	if raceEnabled {
		cmd = exec.Command("go", "build", "-race", "-o", exe, pkg)
	} else {
		cmd = exec.Command("go", "build", "-o", exe, pkg)
	}
	ctx.test.Log("exec:", cmd.Args)

	out, err := cmd.CombinedOutput()
	if err != nil {
		ctx.test.Error(string(out))
		ctx.test.Fatal(err)
	}

	return exe
}

// CompileShared compiles pkg as c-shared.
// TODO: support inclusion from other directories
//  (cgo header paths are currently relative to package root)
func (ctx *Context) CompileShared(t *testing.T, name string, pkg string) Include {
	t.Helper()

	base := ctx.File("build", name)

	// not using race detector for c-shared
	cmd := exec.Command("go", "build", "-buildmode", "c-shared", "-o", base+".so", pkg)
	t.Log("exec:", cmd.Args)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Error(string(out))
		t.Fatal(err)
	}
	t.Log(string(out))

	return Include{Header: base + ".h", Library: base + ".so"}
}

// CompileC compiles file as with gcc and adds the includes.
func (ctx *Context) CompileC(t *testing.T, opts CompileCOptions) string {
	t.Helper()

	exe := ctx.File("build", opts.Dest+".exe")

	var args = []string{}
	if !opts.NoWarn {
		args = append(args, "-Wall")
	}
	args = append(args, "-ggdb")
	args = append(args, "-o", exe)
	for _, inc := range opts.Includes {
		if inc.Header != "" {
			args = append(args, "-I", filepath.Dir(inc.Header))
		}
		if inc.Library != "" {
			if inc.Standard {
				args = append(args,
					"-l"+inc.Library,
				)
				continue
			}
			if runtime.GOOS == "windows" {
				args = append(args,
					"-L"+filepath.Dir(inc.Library),
					"-l:"+filepath.Base(inc.Library),
				)
			} else {
				args = append(args, inc.Library)
			}
		}
	}
	args = append(args, opts.Sources...)

	cmd := exec.Command("gcc", args...)
	t.Log("exec:", cmd.Args)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Error(string(out))
		t.Fatal(err)
	}
	t.Log(string(out))

	return exe
}

// Include defines an includable library for gcc.
type Include struct {
	Header   string
	Library  string
	Standard bool
}
