package ui

import "embed"

// Dist contains the built frontend assets served by the Go backend.
//
//go:embed dist/*
var Dist embed.FS
