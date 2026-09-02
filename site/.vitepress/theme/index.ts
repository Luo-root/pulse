import DefaultTheme from 'vitepress/theme'
import Home from './components/Home.vue'
import './custom.css'

export default {
  extends: DefaultTheme,
  enhanceApp({ app }) {
    // 全局注册自定义首页组件：md 页面直接 <Home /> 使用
    //（md 内 import '.vitepress/…' 相对路径在 rollup 虚拟模块下不可解析，故走全局注册）
    app.component('Home', Home)
  },
}
