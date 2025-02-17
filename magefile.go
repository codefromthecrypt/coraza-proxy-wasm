// Copyright The OWASP Coraza contributors
// SPDX-License-Identifier: Apache-2.0

//go:build mage
// +build mage

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
	"github.com/tetratelabs/wabin/binary"
	"github.com/tetratelabs/wabin/wasm"
)

var addLicenseVersion = "04bfe4ee9ca5764577b029acc6a1957fd1997153" // https://github.com/google/addlicense
var golangCILintVer = "v1.48.0"                                    // https://github.com/golangci/golangci-lint/releases
var gosImportsVer = "v0.3.1"                                       // https://github.com/rinchsan/gosimports/releases/tag/v0.3.1

var errCommitFormatting = errors.New("files not formatted, please commit formatting changes")
var errNoGitDir = errors.New("no .git directory found")

// Format formats code in this repository.
func Format() error {
	if err := sh.RunV("go", "mod", "tidy"); err != nil {
		return err
	}
	// addlicense strangely logs skipped files to stderr despite not being erroneous, so use the long sh.Exec form to
	// discard stderr too.
	if _, err := sh.Exec(map[string]string{}, io.Discard, io.Discard, "go", "run", fmt.Sprintf("github.com/google/addlicense@%s", addLicenseVersion),
		"-c", "The OWASP Coraza contributors",
		"-s=only",
		"-y=",
		"-ignore", "**/*.yml",
		"-ignore", "**/*.yaml",
		"-ignore", "examples/**", "."); err != nil {
		return err
	}
	return sh.RunV("go", "run", fmt.Sprintf("github.com/rinchsan/gosimports/cmd/gosimports@%s", gosImportsVer),
		"-w",
		"-local",
		"github.com/corazawaf/coraza-proxy-wasm",
		".")
}

// Lint verifies code quality.
func Lint() error {
	if err := sh.RunV("go", "run", fmt.Sprintf("github.com/golangci/golangci-lint/cmd/golangci-lint@%s", golangCILintVer), "run"); err != nil {
		return err
	}

	mg.SerialDeps(Format)

	if sh.Run("git", "diff", "--exit-code") != nil {
		return errCommitFormatting
	}

	return nil
}

// Test runs all unit tests.
func Test() error {
	return sh.RunV("go", "test", "./...")
}

// Coverage runs tests with coverage and race detector enabled.
func Coverage() error {
	if err := os.MkdirAll("build", 0755); err != nil {
		return err
	}
	if err := sh.RunV("go", "test", "-race", "-coverprofile=build/coverage.txt", "-covermode=atomic", "-coverpkg=./...", "./..."); err != nil {
		return err
	}

	return sh.RunV("go", "tool", "cover", "-html=build/coverage.txt", "-o", "build/coverage.html")
}

// Doc runs godoc, access at http://localhost:6060
func Doc() error {
	return sh.RunV("go", "run", "golang.org/x/tools/cmd/godoc@latest", "-http=:6060")
}

// Check runs lint and tests.
func Check() {
	mg.SerialDeps(Lint, Test)
}

// Build builds the Coraza wasm plugin.
func Build() error {
	if err := os.MkdirAll("build", 0755); err != nil {
		return err
	}

	timingBuildTag := ""
	if os.Getenv("TIMING") == "true" {
		timingBuildTag = "-tags='timing proxywasm_timing'"
	}

	if err := sh.RunV("tinygo", "build", "-opt=2", "-o", filepath.Join("build", "mainraw.wasm"), "-scheduler=none", "-target=wasi", timingBuildTag); err != nil {
		return err
	}

	return stubUnusedWasmImports(filepath.Join("build", "mainraw.wasm"), filepath.Join("build", "main.wasm"))
}

// UpdateLibs updates the C++ filter dependencies.
func UpdateLibs() error {
	libs := []string{"aho-corasick", "libinjection", "re2"}
	for _, lib := range libs {
		if err := sh.RunV("docker", "build", "-t", "ghcr.io/corazawaf/coraza-proxy-wasm/buildtools-"+lib, filepath.Join("buildtools", lib)); err != nil {
			return err
		}
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		if err := sh.RunV("docker", "run", "-it", "--rm", "-v", fmt.Sprintf("%s:/out", filepath.Join(wd, "lib")), "ghcr.io/corazawaf/coraza-proxy-wasm/buildtools-"+lib); err != nil {
			return err
		}
	}
	return nil
}

// E2e runs e2e tests with a built plugin against the example deployment. Requires docker-compose.
func E2e() error {
	return sh.RunV("docker-compose", "-f", "e2e/docker-compose.yml", "up", "--abort-on-container-exit", "tests")
}

// Ftw runs ftw tests with a built plugin and Envoy. Requires docker-compose.
func Ftw() error {
	if err := sh.RunV("docker-compose", "--file", "ftw/docker-compose.yml", "build"); err != nil {
		return err
	}
	defer func() {
		_ = sh.RunV("docker-compose", "--file", "ftw/docker-compose.yml", "down", "-v")
	}()
	env := map[string]string{
		"FTW_CLOUDMODE": os.Getenv("FTW_CLOUDMODE"),
	}
	if os.Getenv("ENVOY_NOWASM") == "true" {
		env["ENVOY_CONFIG"] = "/conf/envoy-config-nowasm.yaml"
	}
	return sh.RunWithV(env, "docker-compose", "--file", "ftw/docker-compose.yml", "run", "--rm", "ftw")
}

// RunExample spins up the test environment, access at http://localhost:8080. Requires docker-compose.
func RunExample() error {
	return sh.RunV("docker-compose", "--file", "example/docker-compose.yml", "up", "-d", "envoy-logs")
}

// TeardownExample tears down the test environment. Requires docker-compose.
func TeardownExample() error {
	return sh.RunV("docker-compose", "--file", "example/docker-compose.yml", "down")
}

var Default = Build

func stubUnusedWasmImports(inPath, outPath string) error {
	raw, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	mod, err := binary.DecodeModule(raw, wasm.CoreFeaturesV2)
	if err != nil {
		return err
	}

	for _, imp := range mod.ImportSection {
		switch {
		case imp.Name == "fd_filestat_get":
			imp.Name = "fd_fdstat_get"
		case imp.Name == "path_filestat_get":
			imp.Module = "env"
			imp.Name = "proxy_get_header_map_value"
		}
	}

	out := binary.EncodeModule(mod)
	if err = os.WriteFile(outPath, out, 0644); err != nil {
		return err
	}

	return nil
}
