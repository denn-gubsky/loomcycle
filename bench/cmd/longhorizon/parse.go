package main

import "regexp"

var (
	intRe = regexp.MustCompile(`-?\d+`)
	keyRe = regexp.MustCompile(`counter_\d+`)
)

// firstInt returns the first integer token in s (as its canonical string), or "".
// Tolerates a model that answers "The value is 42." instead of "42", and — because
// a model may echo the counter name — strips counter_NN tokens first so
// "counter_01 = 42" grades as 42, not 01.
func firstInt(s string) string {
	return intRe.FindString(keyRe.ReplaceAllString(s, ""))
}

// firstKey returns the first counter_NN token in s, or "".
func firstKey(s string) string {
	return keyRe.FindString(s)
}
