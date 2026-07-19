<script lang="ts">
	import { onMount } from 'svelte';
	import favicon from '$lib/assets/favicon.svg';
	import { page } from '$app/state';
	import { client } from '$lib/api';
	import '../app.css';

	let { children } = $props();
	let cpVersion = $state('');

	function active(path: string): boolean {
		const p = page.url.pathname;
		if (path === '/') return p === '/';
		return p.startsWith(path);
	}

	onMount(() => {
		client
			.getControlPlaneVersion({})
			.then((v) => {
				cpVersion = v.version || 'dev';
			})
			.catch(() => {
				cpVersion = '';
			});
	});
</script>

<svelte:head>
	<link rel="icon" href={favicon} />
	<title>Strategon</title>
</svelte:head>

<div class="shell">
	<header class="topbar">
		<a class="brand" href="/">Strategon</a>
		<nav class="nav">
			<a href="/" class:active={active('/')}>Fleet</a>
			<a href="/deploy" class:active={active('/deploy')}>Deploy</a>
			<a href="/schedules" class:active={active('/schedules')}>Schedules</a>
			<a href="/audit" class:active={active('/audit')}>Audit</a>
		</nav>
		{#if cpVersion}
			<span class="cp-version mono muted" title="Control plane build version (display only)">
				control plane {cpVersion}
			</span>
		{/if}
	</header>
	{@render children()}
</div>

<style>
	.cp-version {
		margin-left: auto;
		font-size: 0.75rem;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
		max-width: 16rem;
	}
</style>
