// Package webui embeds the panel and setup wizard assets.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed panelweb
var panel embed.FS

// Panel returns the files used by the live and sample panels.
func Panel() fs.FS {
	files, err := fs.Sub(panel, "panelweb")
	if err != nil {
		panic(err)
	}
	return files
}

//go:embed wizard/wizard.css
var WizardCSS string

//go:embed wizard/wizard.js
var WizardJS string
