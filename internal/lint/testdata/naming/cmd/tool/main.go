// Command tool is a main package, so the rules skip it.
package main

// WithFlag would break the With rule in a public package.
func WithFlag() {}

func main() { WithFlag() }
