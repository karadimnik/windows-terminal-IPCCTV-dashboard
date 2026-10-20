//go:build !windows

package main

import "fmt"

func main() { fmt.Println("sp2 is a Windows-only experiment (console close events)") }
