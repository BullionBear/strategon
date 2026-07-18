<script lang="ts">
	import favicon from '$lib/assets/favicon.svg';
	import { page } from '$app/state';
	import '../app.css';

	let { children } = $props();

	function active(path: string): boolean {
		const p = page.url.pathname;
		if (path === '/') return p === '/';
		return p.startsWith(path);
	}
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
	</header>
	{@render children()}
</div>
