package main

import (
	"fmt"
	"os"

	_ "net/http/pprof"

	"github.com/internetarchive/Zeno/cmd/v2"
)

func main() {
	if err := cmd.Run(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
