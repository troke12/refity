export function formatTB(tb) {
  if (!tb && tb !== 0) return '0 TB';
  
  if (tb < 0.001) {
    return `${(tb * 1024).toFixed(2)} GB`;
  }
  
  return `${tb.toFixed(2)} TB`;
}
