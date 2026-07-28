export function downloadTextFile(fileName: string, text: string, mimeType = 'text/plain'): void {
  if (typeof document === 'undefined' || typeof URL.createObjectURL !== 'function') {
    throw new Error('File download is unavailable in this environment.')
  }

  const blob = new Blob([text], { type: `${mimeType};charset=utf-8` })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = fileName
  anchor.style.display = 'none'
  document.body.appendChild(anchor)
  anchor.click()
  document.body.removeChild(anchor)
  URL.revokeObjectURL(url)
}
