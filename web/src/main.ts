import { createPinia } from 'pinia'
import { createApp } from 'vue'

import App from '@/App.vue'
import { installAuthGuard, router } from '@/router'
import '@/styles/global.css'

const app = createApp(App)

const pinia = createPinia()
app.use(pinia)
// 闸门要在安装 router 之前挂上：否则首次导航会在没有守卫的情况下放行。
installAuthGuard(pinia)
app.use(router)
app.mount('#app')
