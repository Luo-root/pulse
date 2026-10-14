<script setup lang="ts">
import { useData, withBase } from 'vitepress'

const { frontmatter } = useData()
const href = (value: string) => value.startsWith('/') ? withBase(value) : value
</script>

<template>
  <main class="docs-home">
    <section class="docs-hero" aria-labelledby="home-title">
      <div class="docs-hero-copy">
        <p class="docs-eyebrow">{{ frontmatter.home.eyebrow }}</p>
        <h1 id="home-title">{{ frontmatter.home.title }}</h1>
        <p class="docs-lead">{{ frontmatter.home.lead }}</p>
        <div class="docs-actions">
          <a class="docs-button primary" :href="href(frontmatter.home.primary.link)">{{ frontmatter.home.primary.text }} <span aria-hidden="true">→</span></a>
          <a class="docs-button" :href="href(frontmatter.home.secondary.link)">{{ frontmatter.home.secondary.text }}</a>
        </div>
        <p class="docs-version">{{ frontmatter.home.version }}</p>
      </div>
      <div class="docs-flow" :aria-label="frontmatter.home.flowLabel">
        <p class="docs-flow-title">{{ frontmatter.home.flowTitle }}</p>
        <ol>
          <li v-for="item in frontmatter.home.flow" :key="item.name">
            <span class="docs-flow-dot" aria-hidden="true"></span>
            <div><span class="docs-flow-name">{{ item.name }}</span><span class="docs-flow-detail">{{ item.detail }}</span></div>
            <span class="docs-state">{{ item.state }}</span>
          </li>
        </ol>
        <p class="docs-flow-caption">{{ frontmatter.home.flowCaption }}</p>
      </div>
    </section>
    <section class="docs-paths" aria-labelledby="paths-title">
      <p class="docs-eyebrow">{{ frontmatter.home.pathsLabel }}</p>
      <h2 id="paths-title">{{ frontmatter.home.pathsTitle }}</h2>
      <div class="docs-path-grid">
        <a v-for="path in frontmatter.home.paths" :key="path.title" class="docs-path" :href="href(path.link)">
          <h3>{{ path.title }}</h3><p>{{ path.description }}</p><span>{{ path.action }} <span aria-hidden="true">→</span></span>
        </a>
      </div>
    </section>
    <aside class="docs-companion">
      <div><h2>{{ frontmatter.home.companion.title }}</h2><p>{{ frontmatter.home.companion.description }}</p></div>
      <a :href="href(frontmatter.home.companion.link)">{{ frontmatter.home.companion.action }} <span aria-hidden="true">↗</span></a>
    </aside>
  </main>
</template>
