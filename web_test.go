package main

import "testing"

func TestSearchParsers(t *testing.T) {
	if got := ddgTarget("//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fa%3Fb%3D1&rut=x"); got != "https://example.com/a?b=1" {
		t.Fatal(got)
	}
	if got := bingTarget("https://www.bing.com/ck/a?!&&p=abc&u=a1aHR0cHM6Ly9leGFtcGxlLmNvbS9wYWdl&ntb=1"); got != "https://example.com/page" {
		t.Fatal(got)
	}
	blocks := splitBlocks(`<div class="result results_links web-result"><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fa.com">A <b>t</b></a><a class="result__snippet" href="x">snip <b>here</b></a></div><div class="result result--ad"><a class="result__a" href="https://duckduckgo.com/y.js?ad">Ad</a></div>`, ddgBlockRe)
	if len(blocks) != 2 {
		t.Fatal(len(blocks))
	}
	m := ddgTitleRe.FindStringSubmatch(blocks[0])
	if m == nil || inlineText(m[2]) != "A t" {
		t.Fatal(m)
	}
	if sm := ddgSnipRe.FindStringSubmatch(blocks[0]); sm == nil || inlineText(sm[1]) != "snip here" {
		t.Fatal(sm)
	}
	_, lines, _ := htmlToText(`<html><head><title>T</title></head><body><nav>menu</nav><main><h2 data-x="a->b">Head</h2><p>One <a href="/x">two</a></p><pre>a
  b</pre><table><tr><td>c1</td><td>c2</td></tr></table></main></body></html>`, nil)
	t.Logf("%q", lines)
}

func TestSignal(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		if out, err := quietCmd("git", args...).CombinedOutput(); err != nil {
			t.Fatal(args, string(out))
		}
	}
	run("init", "-q", "--bare", dir+"/remote.git")
	run("init", "-q", dir+"/repo")
	run("-C", dir+"/repo", "remote", "add", "origin", dir+"/remote.git")
	cfg.NotifyRepo = dir + "/repo"
	defer func() { cfg.NotifyRepo = "" }()
	for _, m := range []string{"ts A done: one", "ts B failed: two"} {
		if err := sendSignal(m); err != nil {
			t.Fatal(err)
		}
	}
	out, _ := quietCmd("git", "-C", dir+"/remote.git", "log", "--format=%s", "signals").CombinedOutput()
	if string(out) != "ts B failed: two\nts A done: one\n" {
		t.Fatalf("%q", out)
	}
}
