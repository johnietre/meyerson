package main

import (
  "os"

  "github.com/johnietre/meyerson"
)

func main() {
  meyerson.Run(os.Args[1:])
}
