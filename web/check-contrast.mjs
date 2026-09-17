#!/usr/bin/env node
/*
 * 对比度检查：直接解析 styles/base.css 里的变量，断言 WCAG AA。
 *
 * 为什么值得留成脚本：取值凭手感调是这类深色面板最容易出的问题，
 * 而且改一处颜色往往只想到它自己，忘了它还会落在 --bg-raised 和
 * --bg-inset 上。读真实 CSS 而不是复制一份常量，才不会两边漂移。
 *
 * 小字（< 18.66px 粗体 / < 24px 常规）要求 4.5:1。
 * 本项目绝大多数文字是 10–13px，因此一律按 4.5:1 判。
 *
 * 用法：node check-contrast.mjs
 */

import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const css = readFileSync(join(here, 'src', 'styles', 'base.css'), 'utf8')

/** 从 :root 块里取出某个变量的十六进制值。 */
function token(name) {
  const m = css.match(new RegExp(`--${name}:\\s*(#[0-9a-fA-F]{6})`))
  if (!m) throw new Error(`base.css 里找不到 --${name} 的十六进制值`)
  return m[1]
}

function channel(c) {
  const v = c / 255
  return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4
}

function luminance(hex) {
  const h = hex.replace('#', '')
  return (
    0.2126 * channel(parseInt(h.slice(0, 2), 16)) +
    0.7152 * channel(parseInt(h.slice(2, 4), 16)) +
    0.0722 * channel(parseInt(h.slice(4, 6), 16))
  )
}

function contrast(a, b) {
  const la = luminance(a)
  const lb = luminance(b)
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05)
}

const BG = { '--bg': token('bg'), '--bg-raised': token('bg-raised'), '--bg-inset': token('bg-inset') }

// 前景色 → 它可能落在哪些背景上。
const FG = {
  '--text': Object.keys(BG),
  '--text-dim': Object.keys(BG),
  '--text-faint': Object.keys(BG),
  '--accent': ['--bg', '--bg-raised'],
  '--warn': ['--bg', '--bg-raised'],
  '--ok': ['--bg', '--bg-raised'],
  '--sleep': ['--bg', '--bg-raised'],
}

// 按钮：深色字落在强调色实底上。
const INVERTED = [{ fg: token('bg'), bg: token('accent'), label: '按钮字 on --accent' }]

const MIN = 4.5
let failed = 0
let checked = 0

function report(label, fg, bg, ratio) {
  checked++
  const ok = ratio >= MIN
  if (!ok) failed++
  console.log(
    `${ok ? 'PASS' : 'FAIL'}  ${label.padEnd(34)} ${ratio.toFixed(2)}:1` +
      (ok ? '' : `  < ${MIN}:1`),
  )
}

console.log(`WCAG AA 检查（小字阈值 ${MIN}:1）\n`)

for (const [fgName, bgNames] of Object.entries(FG)) {
  const fg = token(fgName.replace('--', ''))
  for (const bgName of bgNames) {
    report(`${fgName} on ${bgName}`, fg, BG[bgName], contrast(fg, BG[bgName]))
  }
}

for (const { fg, bg, label } of INVERTED) {
  report(label, fg, bg, contrast(fg, bg))
}

console.log(`\n${checked - failed}/${checked} 通过`)
if (failed > 0) {
  console.error(`\n有 ${failed} 组不满足 WCAG AA。调整 base.css 的变量取值。`)
  process.exit(1)
}
