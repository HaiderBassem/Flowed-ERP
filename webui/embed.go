// Package webui carries the built operator interface into the binary.
//
// Embedded rather than served from disk for the reason the previous interface
// gives: a deployment is one file, and a university that has to copy a
// directory of assets beside a binary is a university whose UI is one rsync
// away from disagreeing with its API.
//
// What is embedded is build output rather than source, so a binary is only as
// current as the last `make ui-build`. The directory is kept in the tree by a
// .gitkeep so that `go build` never fails on a fresh clone; a binary built
// before the interface was serves NotBuiltNotice, which says so in as many
// words instead of returning a blank 404 that looks like a routing fault.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Assets is the interface as served, rooted at the build output so that a
// request for /app/assets/index.js resolves to the file of that name.
func Assets() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Unreachable: the directory is embedded above and its absence would
		// have failed the build.
		panic(err)
	}
	return sub
}

// Built reports whether this binary carries a built interface.
func Built() bool {
	_, err := fs.Stat(Assets(), "index.html")
	return err == nil
}

// NotBuiltNotice is served in place of the shell when the interface has not
// been built into this binary.
//
// Deliberately a plain document with no stylesheet and no script: it has to
// render under the same 'self' Content-Security-Policy as the interface, and
// its whole job is to work when nothing else has been built.
const NotBuiltNotice = `<!doctype html>
<html lang="ar" dir="rtl"><head><meta charset="utf-8">
<title>الواجهة غير مبنية</title></head>
<body>
<h1>واجهة المشغّل غير مبنية في هذا الملف التنفيذي</h1>
<p>الـ API يعمل. ما ينقص هو حزمة الواجهة، وتُبنى بأمر واحد:</p>
<pre>make ui-build</pre>
<p>ثم أعد بناء الخادم وتشغيله، لأن الواجهة تُضمَّن داخل الملف التنفيذي:</p>
<pre>make run</pre>
</body></html>`
