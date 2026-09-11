// Package web 打包控制台静态资源。
package web

import _ "embed"

//go:embed index.html
var IndexHTML string
