// 同步脚本：把仓库各包的双语 README 转换为 VitePress 页面（单源 = README）。
// 产出目录在 site/.gitignore 中排除，构建时（本地 dev/build 与 CI）重新生成。
// 规则：
//   - README_zh.md → site/zh/packages/<relDir>/index.md
//     README.md    → site/en/packages/<relDir>/index.md
//   - 根 README 不收录（站点指南页手工覆盖其内容，避免双份维护）
//   - 剥离首行双语导航行
//   - 链接改写：
//       * 指向其他包 README（README_zh.md / README.md）→ 站点路由
//         zh: /pulse/packages/<pkg>/    en: /pulse/en/packages/<pkg>/
//       * 其余相对链接（.go、docs/design/*.md 等）→ GitHub blob 绝对链接
//       * http(s):// 与 #anchor 原样保留
import { readdirSync, statSync, mkdirSync, writeFileSync, readFileSync, existsSync, rmSync } from 'node:fs'
import { dirname, join, relative, resolve, posix } from 'node:path'
import { fileURLToPath } from 'node:url'

const siteDir = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const repoDir = resolve(siteDir, '..')
const GITHUB = 'https://github.com/Luo-root/pulse/blob/main'
const BASE = '/pulse/'

const NAV_LINE = /^\[English\]\(README\.md\) \| \[中文\]\(README_zh\.md\)\s*$/

function* walk(dir) {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    const st = statSync(p)
    if (st.isDirectory()) {
      if (name === '.git' || name === 'node_modules' || p === siteDir) continue
      yield* walk(p)
    } else {
      yield p
    }
  }
}

function rewriteLinks(md, srcRelDir, lang, pageDirs) {
  // 处理 [text](target) 行内链接（不含图片 ![…]；图片统一走 GitHub，下方一并处理）
  // pageDirs：该语言已有站点页面的包目录集合（相对仓库根的 POSIX 路径）
  return md.replace(/(!?)\[((?:[^\]\\\n]|\[[^\]]*\])*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)/g, (full, bang, text, target) => {
    if (/^(https?:)?\/\//.test(target) || target.startsWith('#') || target.startsWith('mailto:')) {
      return full
    }
    // 剥离锚点，锚点只对站点内路由保留（GitHub 链接保留锚点）
    const [pathPart, ...anchorParts] = target.split('#')
    const anchor = anchorParts.length ? `#${anchorParts.join('#')}` : ''
    // 解析相对路径 → 仓库相对路径
    const absPosix = posix.normalize(posix.join(srcRelDir.replaceAll('\\', '/'), pathPart))
    if (absPosix.startsWith('../')) {
      // 超出仓库根（不应发生）：原样保留
      return full
    }
    const pkgPrefix = lang === 'en' ? `${BASE}en/packages/` : `${BASE}packages/`
    const isReadme = /(^|\/)README(_zh)?\.md$/.test(absPosix)
    if (isReadme) {
      // 包 README 互引 → 站点路由（根 README → 站点首页）
      const dir = posix.dirname(absPosix)
      if (dir === '.') {
        return `${bang}[${text}](${lang === 'en' ? BASE + 'en/' : BASE}${anchor})`
      }
      return `${bang}[${text}](${pkgPrefix}${dir}/${anchor})`
    }
    // 目录链接（尾斜杠或无扩展名）：目标目录在本语言有站点页 → 站内路由；否则回退 GitHub
    const looksLikeDir = pathPart.endsWith('/') || !/\.[A-Za-z0-9]+$/.test(pathPart)
    if (looksLikeDir) {
      const dir = absPosix.replaceAll('\\', '/').replace(/\/$/, '')
      if (dir !== '.' && pageDirs.has(dir)) {
        return `${bang}[${text}](${pkgPrefix}${dir}/${anchor})`
      }
      return `${bang}[${text}](${GITHUB}/${absPosix}${anchor})`
    }
    if (bang === '!') {
      // 图片：走 GitHub raw（blob 也能渲染，raw 更直接）
      return `${bang}[${text}](https://raw.githubusercontent.com/Luo-root/pulse/main/${absPosix})`
    }
    // 其他文件 → GitHub blob
    return `${bang}[${text}](${GITHUB}/${absPosix}${anchor})`
  })
}

function firstHeading(md) {
  const m = md.match(/^#\s+(.+)$/m)
  return m ? m[1].trim() : null
}

function convert(srcFile, lang, pageDirs) {
  const rel = relative(repoDir, srcFile)
  const relPosix = rel.replaceAll('\\', '/')
  if (relPosix === 'README.md' || relPosix === 'README_zh.md') return null // 根 README 跳过
  if (!relPosix.endsWith('README.md') && !relPosix.endsWith('README_zh.md')) return null
  // 语言匹配：zh 收 README_zh.md，en 收 README.md
  if (lang === 'zh' && !relPosix.endsWith('README_zh.md')) return null
  if (lang === 'en' && !relPosix.endsWith('README.md')) return null

  let md = readFileSync(srcFile, 'utf8')
  const relDir = posix.dirname(relPosix)
  // 剥导航行
  md = md.split('\n').filter((line, i) => !(i === 0 && NAV_LINE.test(line.trim()))).join('\n')
  // 首个 H1 降级为 frontmatter title（避免每页重复大标题与侧栏冲突）——保留 H1，VitePress 页面需要
  const title = firstHeading(md) ?? relDir
  md = rewriteLinks(md, relDir, lang, pageDirs)
  const fm = `---\ntitle: "${title.replace(/\\/g, '\\\\').replace(/"/g, '\\"')}"\n---\n\n`
  // zh 是 root locale：页面落在 site/packages/...（URL /packages/...）；en 落在 site/en/packages/...
  const dest = join(siteDir, lang === 'en' ? 'en' : '.', 'packages', relDir, 'index.md')
  mkdirSync(dirname(dest), { recursive: true })
  writeFileSync(dest, fm + md, 'utf8')
  return dest
}

// ---- Pass 1：收集双语 README 目录清单（供目录链接的站内路由判定）----
const readmeByLang = { zh: new Map(), en: new Map() } // relDir → srcFile
for (const f of walk(repoDir)) {
  const rel = relative(repoDir, f).replaceAll('\\', '/')
  if (rel === 'README.md' || rel === 'README_zh.md') continue // 根 README 不收录
  if (rel.endsWith('README_zh.md')) readmeByLang.zh.set(posix.dirname(rel), f)
  else if (rel.endsWith('README.md')) readmeByLang.en.set(posix.dirname(rel), f)
}

let n = 0
// 先清理旧生成物（防落点调整 / 源 README 删除后的残留）——保留手写的 packages/index.md 总览页
function cleanGenerated(dir) {
  if (!existsSync(dir)) return
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    if (name === 'index.md') continue // 手写总览页不动
    rmSync(p, { recursive: true, force: true })
  }
}
cleanGenerated(join(siteDir, 'packages'))
cleanGenerated(join(siteDir, 'en', 'packages'))
rmSync(join(siteDir, 'zh'), { recursive: true, force: true })
for (const lang of ['zh', 'en']) {
  const pageDirs = new Set(readmeByLang[lang].keys())
  for (const [, f] of readmeByLang[lang]) {
    if (convert(f, lang, pageDirs)) n++
  }
}
console.log(`sync-docs: wrote ${n} pages from repo READMEs`)
