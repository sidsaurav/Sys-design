// Chapter 8.6 — abuse and operational security.
//
// Four small experiments about what happens after the design is right: callers who use a
// feature as built, a million times; a credential that gets out; a record that somebody
// would like to change; and code that arrives from other people.
//
//	part 1  abuse: an hour of "send me a code by SMS", and six ways to limit it (a model)
//	part 2  least privilege: a pipeline's token leaks. What is it worth, and for how long?
//	part 3  audit: a log that someone with write access wants to alter
//	part 4  supply chain: a dependency is replaced at the registry
//
//	go run ./phase-8-security/code/ch06-ops
//
// Parts 2 and 4 use real HTTP servers on this machine. Nothing else is contacted.
package main

import "fmt"

func commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func main() {
	part1()
	fmt.Println()
	part2()
	fmt.Println()
	part3()
	fmt.Println()
	part4()
}
