// Package all registers all built-in HTTP framework detectors by importing
// their packages for side effects. Import this package in main programs to
// activate the complete set without listing individual frameworks:
//
//	import _ "sauron-eye/internal/graph/entrypoints/frameworks/all"
package all

import (
	_ "sauron-eye/internal/graph/entrypoints/frameworks/echo"
	_ "sauron-eye/internal/graph/entrypoints/frameworks/stdlib"
)
