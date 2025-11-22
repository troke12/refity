export function formatBytes(bytes) {
  if (bytes === 0) {
    return '0 Bytes';
  }
  const unit = 1024;
  if (bytes < unit) {
    return `${bytes} Bytes`;
  }
  const div = Math.floor(Math.log(bytes) / Math.log(unit));
  const units = ['KB', 'MB', 'GB', 'TB', 'PB'];
  return `${(bytes / Math.pow(unit, div)).toFixed(2)} ${units[div - 1]}`;
}

