import { createPinia } from 'pinia'
import { createApp } from 'vue'

import App from './App.vue'
import router from './router'
import './style/main.css'
import { syncThemeWithSystem } from './theme/system-theme'

syncThemeWithSystem()

const app = createApp(App)

app.use(createPinia())
app.use(router)

app.mount('#app')
