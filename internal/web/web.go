// Package web 内嵌前端（go:embed，单 HTML + Alpine.js + ECharts CDN，决策 #2）。
package web

import _ "embed"

//go:embed index.html
var indexHTML []byte

// Index 返回 index.html 内容。
func Index() []byte { return indexHTML }
