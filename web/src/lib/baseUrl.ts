// Where the human API lives, resolved once and shared by api.ts and auth.ts.
//
// Production builds are embedded in and served by the control plane itself, so
// the API is same-origin and the browser's own origin is always correct — no
// domain is baked into the bundle. `vite dev` runs on :5173 while the control
// plane listens on :8081, hence the explicit dev fallback. VITE_API_BASE
// overrides both, for split deployments.
const configured = (typeof import.meta !== 'undefined' && import.meta.env?.VITE_API_BASE) || '';

export const baseUrl =
	configured ||
	(typeof window !== 'undefined' && import.meta.env?.PROD
		? window.location.origin
		: 'http://127.0.0.1:8081');
