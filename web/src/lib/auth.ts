import { baseUrl } from '$lib/baseUrl';

const TOKEN_KEY = 'strategon_access_token';

export type AuthUser = {
	id: string;
	username: string;
	source: string;
	actor: string;
};

export type AuthStatus = {
	mode: 'none' | 'mock' | 'discord' | 'unknown' | string;
	user: AuthUser | null;
	/** Set when /auth/status could not be reached. */
	error?: string;
};

export function getAccessToken(): string | null {
	if (typeof localStorage === 'undefined') return null;
	return localStorage.getItem(TOKEN_KEY);
}

export function setAccessToken(token: string | null) {
	if (typeof localStorage === 'undefined') return;
	if (token) localStorage.setItem(TOKEN_KEY, token);
	else localStorage.removeItem(TOKEN_KEY);
}

export async function fetchAuthStatus(): Promise<AuthStatus> {
	const headers: Record<string, string> = { Accept: 'application/json' };
	const tok = getAccessToken();
	if (tok) headers.Authorization = `Bearer ${tok}`;
	try {
		const res = await fetch(`${baseUrl}/auth/status`, {
			credentials: 'include',
			headers
		});
		if (!res.ok) {
			return {
				mode: 'unknown',
				user: null,
				error: `auth status HTTP ${res.status} (is control plane running with auth?)`
			};
		}
		const data = (await res.json()) as { mode: string; user: AuthUser | null };
		return { mode: data.mode || 'none', user: data.user ?? null };
	} catch (e) {
		return {
			mode: 'unknown',
			user: null,
			error: e instanceof Error ? e.message : String(e)
		};
	}
}

export async function exchangeAuthCode(code: string): Promise<AuthUser | null> {
	const res = await fetch(`${baseUrl}/auth/exchange`, {
		method: 'POST',
		credentials: 'include',
		headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
		body: JSON.stringify({ code })
	});
	if (!res.ok) return null;
	const data = (await res.json()) as { access_token?: string; user?: AuthUser };
	if (data.access_token) setAccessToken(data.access_token);
	return data.user ?? null;
}

/** Consume `#auth_exchange=...` from the URL after Discord/mock login redirect. */
export async function consumeAuthHash(): Promise<boolean> {
	if (typeof window === 'undefined') return false;
	const hash = window.location.hash.startsWith('#')
		? window.location.hash.slice(1)
		: window.location.hash;
	const params = new URLSearchParams(hash);
	const code = params.get('auth_exchange');
	if (!code) return false;
	const user = await exchangeAuthCode(code);
	history.replaceState(null, '', window.location.pathname + window.location.search);
	return !!user;
}

export function loginURL(): string {
	return `${baseUrl}/auth/login`;
}

export async function logout(): Promise<void> {
	setAccessToken(null);
	await fetch(`${baseUrl}/auth/logout`, {
		method: 'POST',
		credentials: 'include',
		headers: { Accept: 'application/json', 'X-Requested-With': 'XMLHttpRequest' }
	}).catch(() => {});
}

export type TokenMeta = {
	id: string;
	name: string;
	user_id: string;
	username: string;
	created_at: string;
	last_used?: string;
};

function authHeaders(): Record<string, string> {
	const headers: Record<string, string> = {
		Accept: 'application/json',
		'Content-Type': 'application/json'
	};
	const tok = getAccessToken();
	if (tok) headers.Authorization = `Bearer ${tok}`;
	return headers;
}

export async function listAPITokens(): Promise<TokenMeta[]> {
	const res = await fetch(`${baseUrl}/auth/tokens`, {
		credentials: 'include',
		headers: authHeaders()
	});
	if (!res.ok) throw new Error(`list tokens: ${res.status}`);
	const data = (await res.json()) as { tokens: TokenMeta[] };
	return data.tokens ?? [];
}

export async function createAPIToken(name: string): Promise<{ token: string; metadata: TokenMeta }> {
	const res = await fetch(`${baseUrl}/auth/tokens`, {
		method: 'POST',
		credentials: 'include',
		headers: authHeaders(),
		body: JSON.stringify({ name })
	});
	if (!res.ok) throw new Error(`create token: ${res.status}`);
	return (await res.json()) as { token: string; metadata: TokenMeta };
}

export async function revokeAPIToken(id: string): Promise<void> {
	const res = await fetch(`${baseUrl}/auth/tokens/${id}`, {
		method: 'DELETE',
		credentials: 'include',
		headers: authHeaders()
	});
	if (!res.ok) throw new Error(`revoke token: ${res.status}`);
}
