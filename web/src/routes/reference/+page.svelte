<script lang="ts">
	import { onMount } from 'svelte';
	import { baseUrl } from '$lib/baseUrl';
	import '@scalar/api-reference/style.css';

	let error = $state('');

	onMount(() => {
		const host = document.getElementById('openapi-docs');
		let api: { destroy?: () => void } | undefined;
		void (async () => {
			if (!host) return;
			try {
				const { createApiReference } = await import('@scalar/api-reference');
				api = createApiReference(host, {
					url: '/openapi.json',
					servers: [{ url: baseUrl, description: 'Control plane (human API)' }],
					layout: 'modern',
					theme: 'default',
					hideClientButton: false,
					showSidebar: true
				}) as { destroy?: () => void };
			} catch (e) {
				error = e instanceof Error ? e.message : String(e);
			}
		})();
		return () => api?.destroy?.();
	});
</script>

<svelte:head>
	<title>Strategon API Reference</title>
</svelte:head>

{#if error}
	<main class="err">
		<p>{error}</p>
		<p><a href="/openapi.json">openapi.json</a> · <a href="/tokens">Back to tokens</a></p>
	</main>
{:else}
	<div id="openapi-docs" class="docs"></div>
{/if}

<style>
	/* Standalone page: strip the app shell atmosphere from app.css. */
	:global(html),
	:global(body) {
		margin: 0;
		min-height: 100%;
		background: #fff !important;
		background-image: none !important;
	}
	:global(body::before) {
		display: none !important;
	}
	.docs {
		min-height: 100vh;
		width: 100%;
	}
	.err {
		font-family: system-ui, sans-serif;
		padding: 2rem;
		max-width: 40rem;
	}
</style>
