// Package jit contains the build-tagged ("security") just-in-time security
// test suite for Azud.
//
// The actual tests live in files guarded by:
//
//	//go:build security
//
// Run them with:
//
//	go test -tags security ./security/jit/...
//
// See README.md in this directory for the threat model, test inventory, and
// interpretation of the VULNERABLE logs.
package jit
