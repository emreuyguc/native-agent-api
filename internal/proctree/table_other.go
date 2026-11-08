//go:build !darwin && !linux

package proctree

import "errors"

func processes() (map[int]process, error) { return nil, errors.New("process table not supported") }
