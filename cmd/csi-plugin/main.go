// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log"
	"os"

	"gitlab.com/subprovisioner/subprovisioner/pkg/csiplugin"
)

func badUsage() {
	fmt.Fprintf(os.Stderr, "usage: %s controller-plugin <image>\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "       %s node-plugin <node_name> <image>\n", os.Args[0])
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		badUsage()
	}

	csiSocketPath := "/run/csi/socket"

	switch os.Args[1] {
	case "controller-plugin":
		if len(os.Args) != 3 {
			badUsage()
		}

		err := csiplugin.RunControllerPlugin(csiSocketPath, os.Args[2])
		if err != nil {
			log.Fatalln(err)
		}

	case "node-plugin":
		if len(os.Args) != 4 {
			badUsage()
		}

		err := csiplugin.RunNodePlugin(csiSocketPath, os.Args[2], os.Args[3])
		if err != nil {
			log.Fatalln(err)
		}

	default:
		badUsage()
	}
}
