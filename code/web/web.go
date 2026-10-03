// Package web holds the dashboard's templates and static files (D13),
// embedded in the API binary so it ships as one file.
package web

import "embed"

// Templates are the dashboard's html/template files.
//
//go:embed templates/*.html
var Templates embed.FS

// Static holds the files served under /ui/static/.
//
//go:embed static
var Static embed.FS
