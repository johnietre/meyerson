package main

import (
)

type Node struct {
  Addr string `json:"addr"`
  IsMaster bool `json:"isMaster"`
  Processes []*Process
  Nodes []*Node `json:"nodes,omitempty"`
}
