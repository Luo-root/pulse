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

function rewriteLinks(md, srcRelDir, lang) {
  // 处理 [text](target) 行内链接（不含图片 ![…]；图片统一走 GitHub blob，下方一并处理）
  return md.replace(/(!?)\[((?:[^\]\\\n]|\[[^\]]*\])*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)/g, (full, bang, text, target) => {
    if (/^(https?:)?\/\//.test(target) || target.startsWith('#') || target.startsWith('mailto:')) {
      return full
    }
    // 剥离锚点，锚点只对站点内路由保留（GitHub 链接保留锚点）
    const [pathPart, ...anchorParts] = target.split('#')
    const anchor = anchorParts.length ? `#${anchorParts.join('#')}` : ''
    // 解析相对路径 → 仓库相对路径
    const absPosix = posix.normalize(posix.join(srcRelDir.replaceAll('\\', '/'), pathPart))
    let repoPath = absPosix
    if (absPosix.startsWith('../')) {
      // 超出仓库根（不应发生）：原样保留
      return full
    }
    const isReadme = /(^|\/)README(_zh)?\.md$/.test(absPosix)
    if (isReadme) {
      // 包 README 互引 → 站点路由（根 README → 站点首页）
      const dir = posix.dirname(absPosix)
      if (dir === '.') {
        return `${bang}[${text}](${lang === 'en' ? BASE + 'en/' : BASE}${anchor})`
      }
      if (lang === 'en') return `${bang}[${text}](${BASE}en/packages/${dir}/${anchor})`
      return `${bang}[${text}](${BASE}packages/${dir}/${anchor})`
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

function convert(srcFile, lang) {
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
  md = rewriteLinks(md, relDir, lang)
  const fm = `---\ntitle: "${title.replace(/\\/g, '\\\\').replace(/"/g, '\\"')}"\n---\n\n`
  // zh 是 root locale：页面落在 site/packages/...（URL /packages/...）；en 落在 site/en/packages/...
  const dest = join(siteDir, lang === 'en' ? 'en' : '.', 'packages', relDir, 'index.md')
  mkdirSync(dirname(dest), { recursive: true })
  writeFileSync(dest, fm + md, 'utf8')
  return dest
}

let n = 0
// 先清理旧生成物（防落点调整 / 源 README 删除后的残留）
for (const stale of [join(siteDir, 'packages'), join(siteDir, 'en', 'packages'), join(siteDir, 'zh')]) {
  rmSync(stale, { recursive: true, force: true })
}
for (const lang of ['zh', 'en']) {
  for (const f of walk(repoDir)) {
    if (convert(f, lang)) n++
  }
}
console.log(`sync-docs: wrote ${n} pages from repo READMEs`)
