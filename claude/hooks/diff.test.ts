import { expect, test } from 'claude-code/testing'

import { parseGitDiff } from './diff'

const DIFF = `diff --git a/main.go b/main.go
index 1..2 100644
--- a/main.go
+++ b/main.go
@@ -1,3 +1,4 @@
 package main
+import "fmt"
-var x = 1
+var x = 2
diff --git a/logo.png b/logo.png
Binary files a/logo.png and b/logo.png differ
`

test('splits a git diff per file with its hunks and counts', () => {
  const files = parseGitDiff(DIFF)
  expect(files.map(f => [f.path, f.added, f.removed, f.note])).toEqual([
    ['main.go', 2, 1, null],
    ['logo.png', 0, 0, 'binary file'],
  ])
  expect(files[0]?.hunks?.startsWith('@@ -1,3 +1,4 @@')).toBe(true)
})

test('a file too large for one Code is cut at a hunk boundary', () => {
  const hunk = `@@ -1 +1 @@\n${'+x\n'.repeat(3000)}`
  const files = parseGitDiff(`diff --git a/big b/big\n--- a/big\n+++ b/big\n${hunk}${hunk}`)
  const shown = files[0]?.hunks ?? ''
  expect(shown.length).toBeLessThan(10_001)
  expect(shown.startsWith('@@')).toBe(true)
})
