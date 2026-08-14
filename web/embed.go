// Package web carries the operator interface into the binary.
//
// Embedded rather than served from disk so a deployment is one file: a
// university that has to copy a directory of assets beside a binary is a
// university whose UI is one rsync away from disagreeing with its API.
package web

import "embed"

// Assets is the interface as served. The test data and this file are excluded
// because neither belongs in a running binary.
//
//go:embed index.html app.css app.js api.js format.js
var Assets embed.FS
