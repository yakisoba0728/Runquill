package main

import "github.com/yakisoba0728/Runquill/internal/app"

var (
	version      = "dev"
	releaseImage string
)

func main() {
	app.Main(version, releaseImage)
}
