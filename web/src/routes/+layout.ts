// Pure SPA: no prerender (pages need a live control plane) and no SSR (nothing
// runs Node in production — the Go binary only serves static files).
export const prerender = false;
export const ssr = false;
