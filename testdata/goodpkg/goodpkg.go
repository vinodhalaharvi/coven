// Package goodpkg is a valid Go package for testing analysis.
package goodpkg

import "fmt"

// Greeting is the exported greeting template.
const Greeting = "Hello, %s!"

// Name is a package-level variable.
var Name = "world"

// Visitor is an exported type.
type Visitor struct {
	ID   int
	Name string
}

// Greet returns a greeting for the given name.
func Greet(who string) string {
	return fmt.Sprintf(Greeting, who)
}

// unexportedHelper should not appear in exports.
func unexportedHelper() int { return 42 }
