//go:build desktop

package main

import (
	"html"
	"strings"
)

// placeholderHTML is the local page shown before the dashboard loads and when
// the daemon cannot be reached. It carries no scripts and its own dark palette,
// so it renders identically in every WebView and needs nothing from the
// network. Its tokens mirror internal/control/web/app.css.
func placeholderHTML(title, detail string) string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1">`)
	b.WriteString(`<style>`)
	b.WriteString(`:root{color-scheme:dark}`)
	b.WriteString(`html,body{height:100%;margin:0}`)
	b.WriteString(`body{background:#0a0d12;color:#e6edf3;`)
	b.WriteString(`font-family:-apple-system,"Segoe UI","PingFang SC","Microsoft YaHei","Noto Sans CJK SC","Source Han Sans SC",system-ui,sans-serif;`)
	b.WriteString(`display:flex;align-items:center;justify-content:center;text-align:center}`)
	b.WriteString(`.card{max-width:44ch;padding:40px}`)
	b.WriteString(`.spinner{width:26px;height:26px;margin:0 auto 22px;border:3px solid #1b2530;border-top-color:#6f94bd;border-radius:50%;animation:spin 900ms linear infinite}`)
	b.WriteString(`h1{font-size:16px;font-weight:600;margin:0 0 10px}`)
	b.WriteString(`p{font-size:13px;line-height:1.6;color:#9aa7b4;margin:0;white-space:pre-wrap}`)
	b.WriteString(`@keyframes spin{to{transform:rotate(360deg)}}`)
	b.WriteString(`</style></head><body><div class="card">`)
	if detail == "" {
		b.WriteString(`<div class="spinner"></div>`)
	}
	b.WriteString(`<h1>`)
	b.WriteString(html.EscapeString(title))
	b.WriteString(`</h1>`)
	if detail != "" {
		b.WriteString(`<p>`)
		b.WriteString(html.EscapeString(detail))
		b.WriteString(`</p>`)
	}
	b.WriteString(`</div></body></html>`)
	return b.String()
}
