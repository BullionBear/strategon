import { createClient, type Client, type Interceptor } from '@connectrpc/connect';
import { createConnectTransport } from '@connectrpc/connect-web';
import { ControlPlaneService } from '$lib/gen/strategyplatform/v1/control_service_pb';
import type { Machine } from '$lib/gen/strategyplatform/v1/control_service_pb';
import { getAccessToken } from '$lib/auth';

const baseUrl =
	(typeof import.meta !== 'undefined' && import.meta.env?.VITE_API_BASE) ||
	'http://127.0.0.1:8081';

const authInterceptor: Interceptor = (next) => async (req) => {
	const tok = getAccessToken();
	if (tok) {
		req.header.set('Authorization', `Bearer ${tok}`);
	}
	return next(req);
};

const transport = createConnectTransport({
	baseUrl,
	interceptors: [authInterceptor],
	// Cookie sessions when UI and API share a site; Bearer still works cross-origin.
	fetch: (input, init) => fetch(input, { ...init, credentials: 'include' })
});

/** Typed Connect client for ControlPlaneService. */
export const client: Client<typeof ControlPlaneService> = createClient(
	ControlPlaneService,
	transport
);

/**
 * Watch a machine with exponential-backoff reconnect. Each event's Machine is
 * the full truth — replace the store, never merge (FRONTEND.md §3.2).
 */
export async function watchMachine(
	machineId: string,
	onEvent: (m: Machine) => void,
	signal: AbortSignal
): Promise<void> {
	let backoff = 500;
	const maxBackoff = 8000;
	while (!signal.aborted) {
		try {
			const stream = client.watchMachine({ machineId }, { signal });
			backoff = 500; // reset on successful connect
			for await (const ev of stream) {
				if (ev.machine) onEvent(ev.machine);
			}
		} catch (err) {
			if (signal.aborted) return;
			console.warn('WatchMachine disconnected, reconnecting', err);
		}
		await sleep(backoff, signal);
		backoff = Math.min(backoff * 2, maxBackoff);
	}
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
	return new Promise((resolve) => {
		const t = setTimeout(resolve, ms);
		signal.addEventListener(
			'abort',
			() => {
				clearTimeout(t);
				resolve();
			},
			{ once: true }
		);
	});
}

export { baseUrl };
