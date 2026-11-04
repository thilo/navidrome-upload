import { Uppy, Dashboard, Tus } from './uppy.min.mjs'

// Navidrome's web UI keeps its login here; this page shares its origin.
const auth = () => ({ 'X-ND-Authorization': `Bearer ${localStorage.getItem('token')}` })
const librarySelect = document.getElementById('library')

// The web UI's theme choice, from its persisted state. Navidrome's Light
// theme gets the light palette, every other theme the dark one.
function navidromeTheme() {
  let choice
  try { choice = JSON.parse(localStorage.getItem('state'))?.theme } catch {}
  if (choice === 'AUTO_THEME_ID') return matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark'
  return choice === 'LightTheme' ? 'light' : 'dark'
}
const theme = navidromeTheme()
document.documentElement.dataset.theme = theme

async function api(path, options = {}) {
  const res = await fetch(`api/${path}`, { ...options, headers: { ...auth(), ...options.headers } })
  const fresh = res.headers.get('X-ND-Authorization')
  if (fresh) localStorage.setItem('token', fresh)
  if (!res.ok) throw Object.assign(new Error(await res.text()), { status: res.status })
  return res.json()
}

function show(article, job, library) {
  const lines = [`${job.state}${job.error ? ': ' + job.error : ''}`]
  if (job.targets.length) lines.push('', 'Imported into:', ...job.targets)
  if (job.scan) lines.push(`Navidrome scan: ${job.scan}`)
  if (job.skipped.length) lines.push('', 'Skipped:', ...job.skipped)
  if (job.log.length) lines.push('', 'beets:', ...job.log)
  article.querySelector('pre').textContent = lines.join('\n')
  article.querySelector('h2').className = job.state === 'failed' ? 'bad' : ''
  article.querySelector('h2').textContent = `Import into ${library.name} — ${job.state}`
}

async function follow(id, library) {
  const article = document.createElement('article')
  article.append(document.createElement('h2'), document.createElement('pre'))
  document.getElementById('imports').prepend(article)
  for (;;) {
    const job = await api(`import/${id}`)
    show(article, job, library)
    if (job.state === 'done' || job.state === 'failed') return
    await new Promise((r) => setTimeout(r, 2000))
  }
}

async function start() {
  let libraries
  try {
    libraries = await api('libraries')
  } catch {
    document.getElementById('login').hidden = false
    return
  }
  for (const l of libraries) librarySelect.add(new Option(l.name, l.id))
  librarySelect.value = localStorage.getItem('upload-library') ?? ''
  if (!librarySelect.value) librarySelect.selectedIndex = 0
  librarySelect.onchange = () => localStorage.setItem('upload-library', librarySelect.value)
  document.getElementById('ready').hidden = false

  const uppy = new Uppy({
    restrictions: {
      maxFileSize: 1 << 30,
      allowedFileTypes: ['.mp3', '.flac', '.m4a', '.ogg', '.opus', '.zip'],
    },
  })
    .use(Dashboard, {
      inline: true, target: '#drop', height: 380, theme,
      fileManagerSelectionType: 'both', proudlyDisplayPoweredByUppy: false,
    })
    .use(Tus, {
      endpoint: 'files/',
      // Also the unit an interrupted upload resumes from.
      chunkSize: 16 << 20,
      retryDelays: [0, 1000, 3000, 10000, 30000],
      removeFingerprintOnSuccess: true,
      onBeforeRequest: (req) => { for (const [k, v] of Object.entries(auth())) req.setHeader(k, v) },
    })

  uppy.on('complete', async ({ successful }) => {
    if (!successful?.length) return
    const library = libraries.find((l) => String(l.id) === librarySelect.value)
    const uploads = successful.map((f) => f.uploadURL.split('/').pop())
    for (const f of successful) uppy.removeFile(f.id)
    try {
      const { id } = await api('import', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          library: library.id,
          uploads,
          // The sidecar starts Navidrome's scan with it once beets is done,
          // also if this page is closed by then.
          subsonic: {
            u: localStorage.getItem('username'),
            t: localStorage.getItem('subsonic-token'),
            s: localStorage.getItem('subsonic-salt'),
          },
        }),
      })
      await follow(id, library)
    } catch (e) {
      uppy.info(`Import failed: ${e.message}`, 'error', 10000)
    }
  })
}

start()
