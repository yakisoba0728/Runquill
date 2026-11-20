// Package examples embeds the public workflow template shipped by Runquill.
package examples

import _ "embed"

//go:embed runquill-first-job.yml
var firstJob string

// FirstJob returns a copy of the starter workflow.
func FirstJob() []byte {
	return []byte(firstJob)
}
