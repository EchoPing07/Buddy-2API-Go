// Package web 内嵌前端（go:embed，单 HTML + 内嵌 Alpine.js，无外部依赖）。
package web

import _ "embed"

//go:embed index.html
var indexHTML []byte

//go:embed alpine.min.js
var alpineJS []byte

// Index 返回 index.html 内容。
func Index() []byte { return indexHTML }

// AlpineJS 返回内嵌的 Alpine.js 运行时。
func AlpineJS() []byte { return alpineJS }
