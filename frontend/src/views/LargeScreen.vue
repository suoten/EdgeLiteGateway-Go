<template>
  <div class="large-screen">
    <!-- 背景装饰：网格 + 光晕 -->
    <div class="ls-bg" aria-hidden="true">
      <div class="ls-bg-grid"></div>
      <div class="ls-bg-glow ls-bg-glow--top"></div>
      <div class="ls-bg-glow ls-bg-glow--bottom"></div>
    </div>

    <!-- Header -->
    <header class="ls-header">
      <div class="ls-header-side ls-header-side--left">
        <span class="ls-header-chip"></span>
        <span class="ls-header-sub">{{ t('largeScreen.systemResource') }}</span>
      </div>
      <div class="ls-header-center">
        <span class="ls-header-deco ls-header-deco--l" aria-hidden="true"></span>
        <h1 class="ls-title">{{ t('largeScreen.title') }}</h1>
        <span class="ls-header-deco ls-header-deco--r" aria-hidden="true"></span>
      </div>
      <div class="ls-header-side ls-header-side--right">
        <div class="ls-clock">
          <div class="ls-clock-date">{{ dateStr }}</div>
          <div class="ls-clock-time">{{ timeStr }}</div>
        </div>
        <button class="ls-fullscreen-btn" type="button" @click="toggleFullscreen">
          {{ isFullscreen ? t('largeScreen.exitFullscreen') : t('largeScreen.enterFullscreen') }}
        </button>
      </div>
    </header>

    <!-- Stats Row -->
    <section class="ls-stats-row">
      <div class="ls-stat-card" v-for="card in statCards" :key="card.label"
        :style="{ '--card-color': card.color }">
        <div class="ls-stat-value">{{ card.display }}</div>
        <div class="ls-stat-label">{{ card.label }}</div>
      </div>
    </section>

    <!-- Charts Row -->
    <section class="ls-charts-row">
      <div class="ls-panel">
        <div class="ls-panel-title">{{ t('largeScreen.deviceStatus') }}</div>
        <div class="ls-panel-body ls-device-body">
          <div class="ls-donut-wrap">
            <v-chart class="ls-donut" :option="donutOption" autoresize />
            <div class="ls-donut-center">
              <div class="ls-donut-num">{{ deviceStats.total }}</div>
              <div class="ls-donut-label">{{ t('largeScreen.totalDevices') }}</div>
            </div>
          </div>
          <div class="ls-donut-side">
            <div class="ls-rate-ring">
              <svg viewBox="0 0 120 120">
                <circle cx="60" cy="60" r="52" fill="none" stroke="rgba(0,229,255,0.12)" stroke-width="8" />
                <circle cx="60" cy="60" r="52" fill="none" stroke="url(#rateGrad)" stroke-width="8"
                  stroke-linecap="round" :stroke-dasharray="`${onlineRatePct * 3.267} 326.7`"
                  transform="rotate(-90 60 60)" />
                <defs>
                  <linearGradient id="rateGrad" x1="0" y1="0" x2="1" y2="1">
                    <stop offset="0%" stop-color="#00e5ff" />
                    <stop offset="100%" stop-color="#00ff9d" />
                  </linearGradient>
                </defs>
              </svg>
              <div class="ls-rate-value">
                <span>{{ onlineRatePct }}</span><em>%</em>
              </div>
              <div class="ls-rate-label">{{ t('largeScreen.deviceOnlineRate') }}</div>
            </div>
            <div class="ls-legend">
              <div class="ls-legend-item">
                <span class="ls-dot" style="background:#00ff9d; box-shadow:0 0 8px rgba(0,255,157,.7)"></span>
                {{ t('largeScreen.online') }}<b>{{ deviceStats.online }}</b>
              </div>
              <div class="ls-legend-item">
                <span class="ls-dot" style="background:#ffb03a; box-shadow:0 0 8px rgba(255,176,58,.7)"></span>
                {{ t('largeScreen.offline') }}<b>{{ deviceStats.offline }}</b>
              </div>
            </div>
          </div>
        </div>
      </div>

      <div class="ls-panel">
        <div class="ls-panel-title">{{ t('largeScreen.dataThroughput') }}</div>
        <div class="ls-panel-body">
          <v-chart v-if="throughputData.length" class="ls-chart" :option="barOption" autoresize />
          <div v-else class="ls-empty">—</div>
        </div>
      </div>

      <div class="ls-panel">
        <div class="ls-panel-title">{{ t('largeScreen.alarmTrend') }}</div>
        <div class="ls-panel-body">
          <v-chart v-if="alarmTrendData.length" class="ls-chart" :option="lineOption" autoresize />
          <div v-else class="ls-empty">—</div>
        </div>
      </div>
    </section>

    <!-- Recent Alarms -->
    <section class="ls-panel ls-alarms-panel">
      <div class="ls-panel-title">{{ t('largeScreen.recentAlarms') }}</div>
      <div class="ls-alarm-list">
        <div v-if="recentAlarms.length === 0" class="ls-empty ls-empty--alarm">—</div>
        <div class="ls-alarm-item" v-for="alarm in recentAlarms" :key="alarm.alarm_id"
          :class="'sev-' + alarm.severity">
          <span class="ls-alarm-sev" :class="'sev-' + alarm.severity">{{ t('alarm.' + alarm.severity) }}</span>
          <span class="ls-alarm-device">{{ alarm.device_id }}</span>
          <span class="ls-alarm-msg">{{ formatAlarmMessage(alarm.message) }}</span>
          <span class="ls-alarm-time">{{ formatDateTime(alarm.fired_at) }}</span>
        </div>
      </div>
    </section>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, reactive, onMounted, onUnmounted } from 'vue'
import { use } from 'echarts/core'
import { PieChart, BarChart, LineChart } from 'echarts/charts'
import { TooltipComponent, LegendComponent, GridComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'
import { graphic } from 'echarts/core'
import VChart from 'vue-echarts'
import http from '@/api/http'
import { t } from '@/i18n'
import { formatDate, formatDateTime, formatTime } from '@/utils/datetime'
import { formatAlarmMessage } from '@/utils/alarmMessage'

use([PieChart, BarChart, LineChart, TooltipComponent, LegendComponent, GridComponent, CanvasRenderer])

// ---- 主题色 ----
const C = {
  cyan: '#00e5ff',
  green: '#00ff9d',
  amber: '#ffb03a',
  red: '#ff5c72',
  purple: '#b388ff',
  blue: '#5aa9ff',
  dim: 'rgba(160,200,235,0.55)',
  axis: 'rgba(120,180,225,0.35)',
  split: 'rgba(90,150,210,0.14)',
  bgPanel: '#071426',
}

// ---- 时钟 ----
const dateStr = ref('')
const timeStr = ref('')

// ---- 数据 ----
const stats = reactive({ devices: 0, online: 0, alarms: 0, points: 0, collections: 0, uptime: 0 })
const display = reactive({ devices: 0, online: 0, alarms: 0, points: 0, collections: 0, uptime: 0 })
const deviceStats = ref({ total: 1, online: 0, offline: 0 })
const throughputData = ref<{ label: string; pct: number }[]>([])
const alarmTrendData = ref<{ date: string; count: number }[]>([])
const recentAlarms = ref<any[]>([])

const prefersReducedMotion =
  typeof window !== 'undefined' &&
  typeof window.matchMedia === 'function' &&
  window.matchMedia('(prefers-reduced-motion: reduce)').matches

// 数字滚动动画
const animFrames: Record<string, number> = {}
function animateTo(key: keyof typeof display, target: number) {
  const from = display[key]
  if (prefersReducedMotion || from === target) {
    display[key] = target
    return
  }
  if (animFrames[key]) cancelAnimationFrame(animFrames[key])
  const start = performance.now()
  const dur = 900
  const step = (now: number) => {
    const p = Math.min((now - start) / dur, 1)
    const eased = 1 - Math.pow(1 - p, 3)
    display[key] = Math.round(from + (target - from) * eased)
    if (p < 1) animFrames[key] = requestAnimationFrame(step)
  }
  animFrames[key] = requestAnimationFrame(step)
}

const statCards = computed(() => [
  { label: t('largeScreen.totalDevices'), display: display.devices, color: C.cyan },
  { label: t('largeScreen.onlineDevices'), display: display.online, color: C.green },
  { label: t('largeScreen.totalPoints'), display: display.points, color: C.amber },
  { label: t('largeScreen.activeAlarms'), display: display.alarms, color: C.red },
  { label: t('largeScreen.collectionsToday'), display: display.collections, color: C.purple },
  { label: t('largeScreen.uptimeDays'), display: display.uptime, color: C.blue },
])

const onlineRatePct = computed(() => {
  const total = deviceStats.value.total || 1
  return Math.round((deviceStats.value.online / total) * 1000) / 10
})

// ---- ECharts 配置 ----
const donutOption = computed(() => ({
  tooltip: {
    trigger: 'item',
    backgroundColor: 'rgba(7,20,38,0.92)',
    borderColor: 'rgba(0,229,255,0.35)',
    textStyle: { color: '#d8f2ff' },
  },
  legend: { show: false },
  series: [
    // 装饰内环
    {
      type: 'pie',
      radius: ['55%', '55.5%'],
      silent: true,
      label: { show: false },
      labelLine: { show: false },
      data: [{ value: 1, itemStyle: { color: 'rgba(0,229,255,0.25)' } }],
    },
    {
      type: 'pie',
      radius: ['66%', '84%'],
      center: ['50%', '50%'],
      avoidLabelOverlap: true,
      itemStyle: {
        borderColor: C.bgPanel,
        borderWidth: 3,
        borderRadius: 8,
      },
      emphasis: {
        scale: true,
        scaleSize: 6,
        itemStyle: { shadowBlur: 22, shadowColor: 'rgba(0,229,255,0.45)' },
      },
      label: { show: false },
      labelLine: { show: false },
      data: [
        {
          value: deviceStats.value.online,
          name: t('largeScreen.online'),
          itemStyle: {
            color: new graphic.LinearGradient(0, 0, 1, 1, [
              { offset: 0, color: '#00e5ff' },
              { offset: 1, color: '#00ff9d' },
            ]),
          },
        },
        {
          value: deviceStats.value.offline,
          name: t('largeScreen.offline'),
          itemStyle: {
            color: new graphic.LinearGradient(0, 0, 1, 1, [
              { offset: 0, color: '#ff9d4d' },
              { offset: 1, color: '#ffb03a' },
            ]),
          },
        },
      ],
    },
  ],
}))

const barOption = computed(() => ({
  tooltip: {
    trigger: 'axis',
    axisPointer: { type: 'shadow', shadowStyle: { color: 'rgba(0,229,255,0.06)' } },
    backgroundColor: 'rgba(7,20,38,0.92)',
    borderColor: 'rgba(0,229,255,0.35)',
    textStyle: { color: '#d8f2ff' },
  },
  grid: { left: 8, right: 8, top: 26, bottom: 4, containLabel: true },
  xAxis: {
    type: 'category',
    data: throughputData.value.map((d) => d.label),
    axisLine: { lineStyle: { color: C.axis } },
    axisTick: { show: false },
    axisLabel: { color: C.dim, fontSize: 11 },
  },
  yAxis: {
    type: 'value',
    axisLine: { show: false },
    axisTick: { show: false },
    splitLine: { lineStyle: { color: C.split } },
    axisLabel: { color: C.dim, fontSize: 11 },
  },
  series: [
    // 背景柱
    {
      type: 'bar',
      barWidth: '42%',
      barGap: '-100%',
      silent: true,
      itemStyle: { color: 'rgba(0,229,255,0.06)', borderRadius: [6, 6, 0, 0] },
      data: throughputData.value.map(() => 100),
    },
    {
      type: 'bar',
      barWidth: '42%',
      itemStyle: {
        borderRadius: [6, 6, 0, 0],
        color: new graphic.LinearGradient(0, 0, 0, 1, [
          { offset: 0, color: '#00e5ff' },
          { offset: 1, color: 'rgba(0,120,255,0.25)' },
        ]),
      },
      emphasis: { itemStyle: { shadowBlur: 14, shadowColor: 'rgba(0,229,255,0.5)' } },
      data: throughputData.value.map((d) => Math.round(d.pct)),
    },
  ],
}))

const lineOption = computed(() => ({
  tooltip: {
    trigger: 'axis',
    backgroundColor: 'rgba(7,20,38,0.92)',
    borderColor: 'rgba(255,92,114,0.4)',
    textStyle: { color: '#d8f2ff' },
  },
  grid: { left: 8, right: 14, top: 26, bottom: 4, containLabel: true },
  xAxis: {
    type: 'category',
    boundaryGap: false,
    data: alarmTrendData.value.map((d) => d.date.slice(5)),
    axisLine: { lineStyle: { color: C.axis } },
    axisTick: { show: false },
    axisLabel: { color: C.dim, fontSize: 11 },
  },
  yAxis: {
    type: 'value',
    minInterval: 1,
    axisLine: { show: false },
    axisTick: { show: false },
    splitLine: { lineStyle: { color: C.split } },
    axisLabel: { color: C.dim, fontSize: 11 },
  },
  series: [
    {
      type: 'line',
      smooth: true,
      symbol: 'circle',
      symbolSize: 6,
      showSymbol: false,
      lineStyle: {
        width: 2.5,
        color: C.red,
        shadowBlur: 12,
        shadowColor: 'rgba(255,92,114,0.55)',
      },
      itemStyle: { color: C.red, borderColor: '#fff', borderWidth: 1 },
      areaStyle: {
        color: new graphic.LinearGradient(0, 0, 0, 1, [
          { offset: 0, color: 'rgba(255,92,114,0.4)' },
          { offset: 1, color: 'rgba(255,92,114,0.02)' },
        ]),
      },
      data: alarmTrendData.value.map((d) => d.count),
    },
  ],
}))

// ---- 数据刷新 ----
async function refresh() {
  try {
    const [statsRes, alarmRes, trendRes] = await Promise.all([
      http.get('/system/stats'),
      http.get('/alarms', { params: { page: 1, size: 12, status: 'firing' } }),
      http.get('/alarms/trend', { params: { days: 7 } }).catch(() => null),
    ])
    const data = statsRes.data?.data ?? {}
    animateTo('devices', data.devices ?? 0)
    animateTo('online', data.online ?? 0)
    animateTo('points', data.points ?? 0)
    animateTo('alarms', data.alarms ?? 0)
    animateTo('collections', data.collections ?? 0)
    animateTo('uptime', data.uptime ?? 0)
    deviceStats.value = {
      total: data.devices ?? 0,
      online: data.online ?? 0,
      offline: (data.devices ?? 0) - (data.online ?? 0),
    }
    recentAlarms.value = (alarmRes.data?.data || []).map((a: any) => ({ ...a }))
    // 真实告警趋势（近 7 天，按日分桶；缺失日期补 0）
    if (trendRes?.data?.data) {
      const byDate = new Map<string, number>(
        (trendRes.data.data as { date: string; count: number }[]).map((d) => [d.date, d.count]),
      )
      alarmTrendData.value = Array.from({ length: 7 }, (_, i) => {
        const day = new Date(Date.now() - (6 - i) * 86400000)
        const key = `${day.getFullYear()}-${String(day.getMonth() + 1).padStart(2, '0')}-${String(day.getDate()).padStart(2, '0')}`
        return { date: key, count: byDate.get(key) ?? 0 }
      })
    }
    // 吞吐占位数据（暂无对应后端小时级接口）
    const now = new Date()
    throughputData.value = Array.from({ length: 12 }, (_, i) => {
      const h = new Date(now.getTime() - (11 - i) * 3600000)
      return { label: `${formatTime(h).slice(0, 2)}h`, pct: Math.random() * 80 + 20 }
    })
  } catch {}
}

// ---- 全屏 ----
const isFullscreen = ref(false)
function onFullscreenChange() {
  isFullscreen.value = !!document.fullscreenElement
}
async function toggleFullscreen() {
  try {
    if (document.fullscreenElement) {
      await document.exitFullscreen()
    } else {
      await document.documentElement.requestFullscreen()
    }
  } catch {}
}

let timer: any
function updateTime() {
  const now = new Date()
  dateStr.value = formatDate(now)
  timeStr.value = formatTime(now)
}

onMounted(() => {
  updateTime()
  refresh()
  timer = setInterval(updateTime, 1000)
  setInterval(refresh, 30000)
  document.addEventListener('fullscreenchange', onFullscreenChange)
})

onUnmounted(() => {
  if (timer) clearInterval(timer)
  document.removeEventListener('fullscreenchange', onFullscreenChange)
})
</script>

<style scoped>
.large-screen {
  position: relative;
  width: 100%;
  height: 100vh;
  min-height: 760px;
  color: #d8f2ff;
  display: flex;
  flex-direction: column;
  padding: 14px 20px 18px;
  gap: 12px;
  overflow: auto;
  background: radial-gradient(1200px 600px at 50% -10%, #0d2a4d 0%, transparent 60%),
    radial-gradient(900px 500px at 85% 110%, #0a2340 0%, transparent 55%),
    linear-gradient(160deg, #040c1c 0%, #061426 55%, #040c1c 100%);
}

/* ---------- 背景 ---------- */
.ls-bg {
  position: fixed;
  inset: 0;
  pointer-events: none;
  z-index: 0;
}
.ls-bg-grid {
  position: absolute;
  inset: 0;
  background-image:
    linear-gradient(rgba(0, 229, 255, 0.045) 1px, transparent 1px),
    linear-gradient(90deg, rgba(0, 229, 255, 0.045) 1px, transparent 1px);
  background-size: 48px 48px;
  mask-image: radial-gradient(ellipse 90% 80% at 50% 40%, #000 30%, transparent 100%);
  -webkit-mask-image: radial-gradient(ellipse 90% 80% at 50% 40%, #000 30%, transparent 100%);
}
.ls-bg-glow {
  position: absolute;
  width: 60vw;
  height: 40vh;
  border-radius: 50%;
  filter: blur(90px);
  opacity: 0.14;
}
.ls-bg-glow--top { top: -18vh; left: 20vw; background: #0aa8ff; }
.ls-bg-glow--bottom { bottom: -20vh; right: 8vw; background: #00c2a8; }

.large-screen > * { position: relative; z-index: 1; }

/* ---------- Header ---------- */
.ls-header {
  display: grid;
  grid-template-columns: 1fr auto 1fr;
  align-items: center;
  padding: 6px 4px 12px;
  position: relative;
}
.ls-header::after {
  content: '';
  position: absolute;
  left: 0;
  right: 0;
  bottom: 0;
  height: 2px;
  background: linear-gradient(90deg,
    transparent 0%, rgba(0, 229, 255, 0.55) 18%,
    rgba(0, 229, 255, 0.9) 50%,
    rgba(0, 229, 255, 0.55) 82%, transparent 100%);
  filter: drop-shadow(0 0 6px rgba(0, 229, 255, 0.6));
}
.ls-header-center {
  display: flex;
  align-items: center;
  gap: 18px;
  min-width: 0;
}
.ls-title {
  margin: 0;
  font-size: 30px;
  font-weight: 700;
  letter-spacing: 6px;
  white-space: nowrap;
  background: linear-gradient(180deg, #eaffff 15%, #6fd8ff 55%, #1e9fe0 100%);
  -webkit-background-clip: text;
  background-clip: text;
  -webkit-text-fill-color: transparent;
  filter: drop-shadow(0 0 12px rgba(0, 200, 255, 0.45));
}
.ls-header-deco {
  flex: 1;
  height: 10px;
  min-width: 60px;
  clip-path: polygon(0 50%, 12px 0, 100% 0, calc(100% - 4px) 100%, 12px 100%);
}
.ls-header-deco--l {
  background: linear-gradient(90deg, transparent, rgba(0, 229, 255, 0.7));
  transform: scaleX(-1);
}
.ls-header-deco--r {
  background: linear-gradient(90deg, transparent, rgba(0, 229, 255, 0.7));
}
.ls-header-side {
  display: flex;
  align-items: center;
  gap: 12px;
  min-width: 0;
}
.ls-header-side--right { justify-content: flex-end; }
.ls-header-sub {
  font-size: 13px;
  letter-spacing: 2px;
  color: rgba(140, 195, 235, 0.75);
}
.ls-header-chip {
  width: 8px;
  height: 8px;
  border-radius: 2px;
  background: #00e5ff;
  box-shadow: 0 0 10px rgba(0, 229, 255, 0.9);
  animation: ls-pulse 2.4s ease-in-out infinite;
}
@keyframes ls-pulse {
  0%, 100% { opacity: 1; box-shadow: 0 0 10px rgba(0, 229, 255, 0.9); }
  50% { opacity: 0.45; box-shadow: 0 0 3px rgba(0, 229, 255, 0.4); }
}
.ls-clock { text-align: right; line-height: 1.25; }
.ls-clock-date {
  font-size: 13px;
  color: rgba(140, 195, 235, 0.8);
  letter-spacing: 1px;
}
.ls-clock-time {
  font-size: 24px;
  font-weight: 700;
  font-variant-numeric: tabular-nums;
  color: #9fe6ff;
  text-shadow: 0 0 14px rgba(0, 200, 255, 0.45);
  letter-spacing: 2px;
}
.ls-fullscreen-btn {
  margin-left: 8px;
  padding: 7px 14px;
  font-size: 13px;
  color: #8fdcff;
  background: rgba(0, 160, 255, 0.1);
  border: 1px solid rgba(0, 200, 255, 0.35);
  border-radius: 4px;
  cursor: pointer;
  transition: background 0.2s, border-color 0.2s, box-shadow 0.2s;
}
.ls-fullscreen-btn:hover {
  background: rgba(0, 180, 255, 0.22);
  border-color: rgba(0, 220, 255, 0.7);
  box-shadow: 0 0 12px rgba(0, 200, 255, 0.35);
}

/* ---------- 指标卡 ---------- */
.ls-stats-row {
  display: grid;
  grid-template-columns: repeat(6, 1fr);
  gap: 12px;
}
.ls-stat-card {
  --card-color: #00e5ff;
  position: relative;
  padding: 16px 14px 14px;
  text-align: center;
  border-radius: 6px;
  background:
    linear-gradient(180deg, rgba(0, 90, 180, 0.16) 0%, rgba(4, 20, 40, 0.5) 100%);
  border: 1px solid rgba(0, 160, 255, 0.18);
  overflow: hidden;
  transition: transform 0.25s ease, box-shadow 0.25s ease, border-color 0.25s ease;
}
.ls-stat-card::before {
  content: '';
  position: absolute;
  top: 0;
  left: 10%;
  right: 10%;
  height: 2px;
  background: linear-gradient(90deg, transparent, var(--card-color), transparent);
  opacity: 0.85;
  filter: drop-shadow(0 0 6px var(--card-color));
}
.ls-stat-card::after {
  content: '';
  position: absolute;
  top: -60%;
  left: -30%;
  width: 40%;
  height: 220%;
  transform: rotate(25deg);
  background: linear-gradient(90deg, transparent, rgba(255, 255, 255, 0.05), transparent);
  animation: ls-sheen 5.5s ease-in-out infinite;
  animation-delay: calc(var(--sheen-delay, 0) * 1s);
}
@keyframes ls-sheen {
  0%, 72% { left: -45%; }
  92%, 100% { left: 115%; }
}
.ls-stat-card:hover {
  transform: translateY(-3px);
  border-color: color-mix(in srgb, var(--card-color) 55%, transparent);
  box-shadow: 0 8px 26px -8px color-mix(in srgb, var(--card-color) 45%, transparent);
}
.ls-stat-value {
  font-size: 34px;
  font-weight: 700;
  line-height: 1.15;
  font-variant-numeric: tabular-nums;
  color: var(--card-color);
  text-shadow: 0 0 16px color-mix(in srgb, var(--card-color) 55%, transparent);
}
.ls-stat-label {
  margin-top: 6px;
  font-size: 13px;
  letter-spacing: 1px;
  color: rgba(150, 200, 235, 0.8);
}

.ls-charts-row {
  display: flex;
  gap: 12px;
  flex: 1;
  min-height: 280px;
}

/* ---------- 面板 ---------- */
.ls-panel {
  position: relative;
  display: flex;
  flex-direction: column;
  flex: 1;
  min-width: 0;
  min-height: 260px;
  padding: 14px 16px;
  border-radius: 6px;
  background: linear-gradient(180deg, rgba(8, 32, 62, 0.55) 0%, rgba(4, 16, 34, 0.72) 100%);
  border: 1px solid rgba(0, 140, 220, 0.22);
  backdrop-filter: blur(4px);
}
/* 四角科技描边 */
.ls-panel::before,
.ls-panel::after {
  content: '';
  position: absolute;
  width: 16px;
  height: 16px;
  pointer-events: none;
}
.ls-panel::before {
  top: -1px;
  left: -1px;
  border-top: 2px solid #00e5ff;
  border-left: 2px solid #00e5ff;
  filter: drop-shadow(0 0 4px rgba(0, 229, 255, 0.7));
}
.ls-panel::after {
  bottom: -1px;
  right: -1px;
  border-bottom: 2px solid #00e5ff;
  border-right: 2px solid #00e5ff;
  filter: drop-shadow(0 0 4px rgba(0, 229, 255, 0.7));
}
.ls-panel-title {
  flex: none;
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 15px;
  font-weight: 600;
  letter-spacing: 2px;
  color: #9fe6ff;
  padding-bottom: 10px;
  margin-bottom: 10px;
  border-bottom: 1px solid rgba(0, 160, 255, 0.16);
  background: linear-gradient(90deg, rgba(0, 229, 255, 0.1), transparent 70%) left / auto 100% no-repeat padding-box bottom;
  position: relative;
}
.ls-panel-title::before {
  content: '';
  width: 4px;
  height: 14px;
  border-radius: 2px;
  background: linear-gradient(180deg, #00e5ff, #0080ff);
  box-shadow: 0 0 8px rgba(0, 229, 255, 0.7);
}
.ls-panel-body {
  position: relative;
  flex: 1;
  min-height: 0;
  display: flex;
  align-items: stretch;
  justify-content: center;
  overflow: hidden;
}
.ls-chart {
  position: absolute !important;
  inset: 4px 8px 8px;
  width: auto !important;
  height: auto !important;
}
.ls-empty {
  width: 100%;
  display: flex;
  align-items: center;
  justify-content: center;
  color: rgba(120, 170, 210, 0.4);
  font-size: 22px;
  letter-spacing: 4px;
}

/* ---------- 设备状态 ---------- */
.ls-device-body { gap: 10px; align-items: center; }
.ls-donut-wrap {
  position: relative;
  width: 52%;
  max-width: 240px;
  aspect-ratio: 1;
  flex: none;
}
.ls-donut { width: 100%; height: 100%; }
.ls-donut-center {
  position: absolute;
  top: 50%;
  left: 50%;
  transform: translate(-50%, -50%);
  text-align: center;
  pointer-events: none;
}
.ls-donut-num {
  font-size: 34px;
  font-weight: 700;
  font-variant-numeric: tabular-nums;
  color: #eaffff;
  text-shadow: 0 0 16px rgba(0, 229, 255, 0.6);
}
.ls-donut-label { font-size: 12px; color: rgba(150, 200, 235, 0.7); margin-top: 2px; }
.ls-donut-side {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 12px;
  min-width: 0;
}
.ls-rate-ring {
  position: relative;
  width: 108px;
  height: 108px;
}
.ls-rate-ring svg { width: 100%; height: 100%; }
.ls-rate-value {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 20px;
  font-weight: 700;
  color: #eaffff;
  text-shadow: 0 0 10px rgba(0, 255, 157, 0.4);
}
.ls-rate-value em { font-style: normal; font-size: 12px; color: #00ff9d; margin-left: 2px; }
.ls-rate-label {
  position: absolute;
  left: 0;
  right: 0;
  bottom: 22%;
  text-align: center;
  font-size: 10px;
  color: rgba(150, 200, 235, 0.65);
}
.ls-legend { display: flex; flex-direction: column; gap: 8px; }
.ls-legend-item {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
  color: rgba(190, 225, 250, 0.85);
}
.ls-legend-item b {
  margin-left: 4px;
  font-variant-numeric: tabular-nums;
  color: #eaffff;
}
.ls-dot { width: 10px; height: 10px; border-radius: 50%; flex: none; }

/* ---------- 告警列表 ---------- */
.ls-alarms-panel { flex: 1.1; min-height: 190px; }
.ls-alarm-list {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
  gap: 6px;
  overflow: auto;
}
.ls-empty--alarm { flex: 1; }
.ls-alarm-item {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 8px 12px;
  border-radius: 4px;
  font-size: 13px;
  background: rgba(0, 60, 120, 0.14);
  border: 1px solid transparent;
  border-left: 3px solid transparent;
  transition: background 0.2s, border-color 0.2s;
  animation: ls-row-in 0.4s ease both;
}
@keyframes ls-row-in {
  from { opacity: 0; transform: translateY(6px); }
  to { opacity: 1; transform: translateY(0); }
}
.ls-alarm-item:hover { background: rgba(0, 100, 190, 0.24); }
.ls-alarm-item.sev-critical { border-left-color: #ff5c72; }
.ls-alarm-item.sev-critical:hover { border-color: rgba(255, 92, 114, 0.4); }
.ls-alarm-item.sev-high { border-left-color: #ff9d4d; }
.ls-alarm-item.sev-high:hover { border-color: rgba(255, 157, 77, 0.4); }
.ls-alarm-item.sev-medium { border-left-color: #ffb03a; }
.ls-alarm-item.sev-low { border-left-color: #5aa9ff; }
.ls-alarm-item.sev-low:hover { border-color: rgba(90, 169, 255, 0.4); }
.ls-alarm-item.sev-medium:hover { border-color: rgba(255, 176, 58, 0.4); }
.ls-alarm-sev {
  flex: none;
  padding: 2px 10px;
  border-radius: 10px;
  font-size: 11px;
  font-weight: 700;
  letter-spacing: 1px;
  text-transform: uppercase;
  border: 1px solid transparent;
}
.ls-alarm-sev.sev-critical { color: #ff8a9b; background: rgba(255, 92, 114, 0.14); border-color: rgba(255, 92, 114, 0.45); }
.ls-alarm-sev.sev-high { color: #ffb27e; background: rgba(255, 157, 77, 0.14); border-color: rgba(255, 157, 77, 0.45); }
.ls-alarm-sev.sev-medium { color: #ffcf6e; background: rgba(255, 176, 58, 0.14); border-color: rgba(255, 176, 58, 0.45); }
.ls-alarm-sev.sev-low { color: #8ec4ff; background: rgba(90, 169, 255, 0.14); border-color: rgba(90, 169, 255, 0.45); }
.ls-alarm-device {
  flex: none;
  color: #9fe6ff;
  font-weight: 600;
  font-size: 12px;
  letter-spacing: 0.5px;
}
.ls-alarm-msg {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: rgba(210, 235, 252, 0.9);
}
.ls-alarm-time {
  flex: none;
  font-variant-numeric: tabular-nums;
  font-size: 12px;
  color: rgba(130, 175, 210, 0.7);
}

/* ---------- 自适应 & 可访问性 ---------- */
@media (max-width: 1200px) {
  .large-screen { height: auto; min-height: 100vh; }
  .ls-stats-row { grid-template-columns: repeat(3, 1fr); }
  .ls-charts-row { flex-direction: column; min-height: 0; }
  .ls-panel { flex: none; min-height: 0; }
  .ls-panel-body { min-height: 240px; }
  .ls-alarms-panel { min-height: 200px; }
}
@media (prefers-reduced-motion: reduce) {
  .large-screen *,
  .large-screen *::before,
  .large-screen *::after {
    animation: none !important;
    transition: none !important;
  }
}
</style>
