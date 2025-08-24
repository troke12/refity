/** @type {import('tailwindcss').Config} */
module.exports = {
  content: [
    "./internal/web/template.go",
    "./cmd/**/*.go",
    "./internal/**/*.go"
  ],
  theme: {
    extend: {},
  },
  plugins: [],
}
