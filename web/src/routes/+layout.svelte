<script lang="ts">
	import { onMount } from 'svelte';
	import favicon from '$lib/assets/favicon.svg';
	import { page } from '$app/state';
	import { client } from '$lib/api';
	import { BP } from '$lib/fleet';
	import {
		hasSidebarCollapsedPreference,
		readSidebarCollapsed,
		writeSidebarCollapsed
	} from '$lib/sidebar';
	import '../app.css';

	let { children } = $props();
	let cpVersion = $state('');
	let collapsed = $state(false);
	let drawerOpen = $state(false);

	function active(path: string): boolean {
		const p = page.url.pathname;
		if (path === '/') return p === '/' || p.startsWith('/machines');
		return p.startsWith(path);
	}

	function toggleCollapse() {
		collapsed = !collapsed;
		writeSidebarCollapsed(collapsed);
	}

	function openDrawer() {
		drawerOpen = true;
	}

	function closeDrawer() {
		drawerOpen = false;
	}

	function onNavClick() {
		if (typeof window !== 'undefined' && window.innerWidth < BP.tabletMin) {
			closeDrawer();
		}
	}

	$effect(() => {
		if (typeof document === 'undefined') return;
		document.body.style.overflow = drawerOpen ? 'hidden' : '';
		return () => {
			document.body.style.overflow = '';
		};
	});

	onMount(() => {
		collapsed = readSidebarCollapsed();
		const w = window.innerWidth;
		if (
			w >= BP.tabletMin &&
			w <= BP.tabletMax &&
			!hasSidebarCollapsedPreference()
		) {
			collapsed = true;
		}

		client
			.getControlPlaneVersion({})
			.then((v) => {
				cpVersion = v.version || 'dev';
			})
			.catch(() => {
				cpVersion = '';
			});

		const onKey = (e: KeyboardEvent) => {
			if (e.key === 'Escape' && drawerOpen) closeDrawer();
		};
		const onResize = () => {
			if (window.innerWidth >= BP.tabletMin) drawerOpen = false;
		};
		window.addEventListener('keydown', onKey);
		window.addEventListener('resize', onResize);
		return () => {
			window.removeEventListener('keydown', onKey);
			window.removeEventListener('resize', onResize);
		};
	});

	const navGroups = [
		{
			label: 'Fleet',
			items: [{ href: '/', label: 'Machines', icon: 'machines' as const }]
		},
		{
			label: 'Deploy',
			items: [
				{ href: '/deploy', label: 'Deploy', icon: 'deploy' as const },
				{ href: '/artifacts', label: 'Artifacts', icon: 'artifacts' as const },
				{ href: '/schedules', label: 'Schedules', icon: 'schedules' as const }
			]
		},
		{
			label: 'Observe',
			items: [{ href: '/audit', label: 'Audit', icon: 'audit' as const }]
		}
	];
</script>

<svelte:head>
	<link rel="icon" href={favicon} />
	<title>Strategon</title>
</svelte:head>

<div class="shell" class:collapsed class:drawer-open={drawerOpen}>
	<header class="mobile-bar">
		<button
			type="button"
			class="btn icon"
			aria-label="Open navigation"
			aria-expanded={drawerOpen}
			onclick={openDrawer}
		>
			<svg width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden="true">
				<path
					d="M3 5h14M3 10h14M3 15h14"
					stroke="currentColor"
					stroke-width="1.75"
					stroke-linecap="round"
				/>
			</svg>
		</button>
		<a class="brand" href="/" onclick={closeDrawer}>Strategon</a>
		{#if cpVersion}
			<span class="cp-version mono muted" title="Control plane build version">
				cp {cpVersion}
			</span>
		{/if}
	</header>

	<button
		type="button"
		class="drawer-backdrop"
		aria-label="Close navigation"
		tabindex={drawerOpen ? 0 : -1}
		onclick={closeDrawer}
	></button>

	<aside class="sidebar" aria-label="Main">
		<div class="sidebar-top">
			<button
				type="button"
				class="btn icon collapse-btn"
				aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
				aria-pressed={collapsed}
				onclick={toggleCollapse}
			>
				<svg width="18" height="18" viewBox="0 0 20 20" fill="none" aria-hidden="true">
					{#if collapsed}
						<path
							d="M7 4l6 6-6 6"
							stroke="currentColor"
							stroke-width="1.75"
							stroke-linecap="round"
							stroke-linejoin="round"
						/>
					{:else}
						<path
							d="M13 4L7 10l6 6"
							stroke="currentColor"
							stroke-width="1.75"
							stroke-linecap="round"
							stroke-linejoin="round"
						/>
					{/if}
				</svg>
			</button>
			<a class="brand brand-full" href="/" onclick={onNavClick}>Strategon</a>
		</div>

		<nav class="sidebar-nav">
			{#each navGroups as group}
				<div class="nav-group">
					<div class="nav-group-label">{group.label}</div>
					{#each group.items as item}
						<a
							href={item.href}
							class="nav-item"
							class:active={active(item.href)}
							title={item.label}
							onclick={onNavClick}
						>
							<span class="nav-icon" aria-hidden="true">
								{#if item.icon === 'machines'}
									<svg width="18" height="18" viewBox="0 0 20 20" fill="none">
										<rect
											x="3"
											y="4"
											width="14"
											height="10"
											rx="1.5"
											stroke="currentColor"
											stroke-width="1.5"
										/>
										<path d="M7 17h6" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
									</svg>
								{:else if item.icon === 'deploy'}
									<svg width="18" height="18" viewBox="0 0 20 20" fill="none">
										<path
											d="M10 3v10M6.5 9.5 10 13l3.5-3.5M4 16h12"
											stroke="currentColor"
											stroke-width="1.5"
											stroke-linecap="round"
											stroke-linejoin="round"
										/>
									</svg>
								{:else if item.icon === 'artifacts'}
									<svg width="18" height="18" viewBox="0 0 20 20" fill="none">
										<path
											d="M4 6.5 10 3l6 3.5v7L10 17l-6-3.5v-7Z"
											stroke="currentColor"
											stroke-width="1.5"
											stroke-linejoin="round"
										/>
										<path d="M4 6.5 10 10l6-3.5M10 10v7" stroke="currentColor" stroke-width="1.5" />
									</svg>
								{:else if item.icon === 'schedules'}
									<svg width="18" height="18" viewBox="0 0 20 20" fill="none">
										<circle cx="10" cy="10" r="7" stroke="currentColor" stroke-width="1.5" />
										<path
											d="M10 6.5V10l2.5 1.5"
											stroke="currentColor"
											stroke-width="1.5"
											stroke-linecap="round"
											stroke-linejoin="round"
										/>
									</svg>
								{:else}
									<svg width="18" height="18" viewBox="0 0 20 20" fill="none">
										<path
											d="M5 5h10M5 10h10M5 15h6"
											stroke="currentColor"
											stroke-width="1.5"
											stroke-linecap="round"
										/>
									</svg>
								{/if}
							</span>
							<span class="nav-label">{item.label}</span>
						</a>
					{/each}
				</div>
			{/each}
		</nav>

		{#if cpVersion}
			<div class="sidebar-foot mono muted" title="Control plane build version (display only)">
				<span class="foot-full">cp {cpVersion}</span>
				<span class="foot-short">{cpVersion.slice(0, 6)}</span>
			</div>
		{/if}
	</aside>

	<main class="shell-main">
		{@render children()}
	</main>
</div>

<style>
	.mobile-bar {
		display: none;
		position: fixed;
		top: 0;
		left: 0;
		right: 0;
		z-index: 40;
		align-items: center;
		gap: 0.65rem;
		height: var(--mobile-bar-h);
		padding: 0 0.75rem;
		background: rgba(255, 255, 255, 0.88);
		backdrop-filter: blur(10px);
		border-bottom: 1px solid var(--line);
	}
	.mobile-bar .brand {
		font-family: var(--display);
		font-size: 1.25rem;
		font-weight: 700;
		color: var(--ink);
		letter-spacing: -0.03em;
		text-decoration: none;
	}
	.mobile-bar .cp-version {
		margin-left: auto;
		font-size: 0.7rem;
		max-width: 8rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.drawer-backdrop {
		display: none;
		position: fixed;
		inset: 0;
		z-index: 45;
		border: none;
		padding: 0;
		margin: 0;
		background: rgba(20, 32, 43, 0.35);
		cursor: pointer;
	}

	.sidebar {
		position: sticky;
		top: 0;
		align-self: flex-start;
		display: flex;
		flex-direction: column;
		width: var(--sidebar-width);
		min-height: 100vh;
		flex-shrink: 0;
		padding: 0.85rem 0.65rem 1rem;
		border-right: 1px solid var(--line);
		background: rgba(255, 255, 255, 0.55);
		backdrop-filter: blur(10px);
		transition: width 0.18s ease;
		z-index: 30;
	}
	:global(.shell.collapsed) .sidebar {
		width: var(--sidebar-collapsed);
		padding-left: 0.4rem;
		padding-right: 0.4rem;
	}

	.sidebar-top {
		display: flex;
		align-items: center;
		gap: 0.35rem;
		min-height: 2.25rem;
		margin-bottom: 1rem;
		padding: 0 0.25rem;
	}
	:global(.shell.collapsed) .sidebar-top {
		flex-direction: column;
		gap: 0.5rem;
	}
	.sidebar .brand {
		font-family: var(--display);
		font-size: 1.35rem;
		font-weight: 700;
		color: var(--ink);
		letter-spacing: -0.03em;
		text-decoration: none;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}
	.sidebar .brand:hover {
		color: var(--accent-ink);
		text-decoration: none;
	}
	:global(.shell.collapsed) .brand-full {
		position: absolute;
		width: 1px;
		height: 1px;
		padding: 0;
		margin: -1px;
		overflow: hidden;
		clip: rect(0, 0, 0, 0);
		border: 0;
	}

	.sidebar-nav {
		display: flex;
		flex-direction: column;
		gap: 1.1rem;
		flex: 1;
	}
	.nav-group {
		display: flex;
		flex-direction: column;
		gap: 0.15rem;
	}
	.nav-group-label {
		font-size: 0.68rem;
		font-weight: 700;
		letter-spacing: 0.06em;
		text-transform: uppercase;
		color: var(--ink-muted);
		padding: 0 0.55rem 0.3rem;
	}
	:global(.shell.collapsed) .nav-group-label {
		display: none;
	}
	.nav-item {
		display: flex;
		align-items: center;
		gap: 0.55rem;
		padding: 0.45rem 0.55rem;
		border-radius: 8px;
		color: var(--ink-muted);
		font-weight: 500;
		font-size: 0.92rem;
		text-decoration: none;
	}
	.nav-item:hover {
		background: rgba(13, 115, 119, 0.08);
		color: var(--accent-ink);
		text-decoration: none;
	}
	.nav-item.active {
		background: rgba(13, 115, 119, 0.12);
		color: var(--accent-ink);
		font-weight: 600;
	}
	:global(.shell.collapsed) .nav-item {
		justify-content: center;
		padding: 0.55rem;
	}
	.nav-icon {
		display: inline-flex;
		flex-shrink: 0;
	}
	.nav-label {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	:global(.shell.collapsed) .nav-label {
		display: none;
	}

	.sidebar-foot {
		margin-top: auto;
		padding: 0.75rem 0.55rem 0.25rem;
		border-top: 1px solid var(--line);
		font-size: 0.72rem;
		word-break: break-all;
	}
	.foot-short {
		display: none;
	}
	:global(.shell.collapsed) .sidebar-foot {
		padding: 0.75rem 0.15rem 0.25rem;
		text-align: center;
	}
	:global(.shell.collapsed) .foot-full {
		display: none;
	}
	:global(.shell.collapsed) .foot-short {
		display: block;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	/* Mobile: morph sidebar into drawer; top bar + hamburger */
	@media (max-width: 639px) {
		.mobile-bar {
			display: flex;
		}
		.drawer-backdrop {
			display: none;
		}
		:global(.shell.drawer-open) .drawer-backdrop {
			display: block;
		}
		.sidebar {
			position: fixed;
			left: 0;
			top: 0;
			bottom: 0;
			z-index: 50;
			width: min(280px, 86vw) !important;
			min-height: 100%;
			transform: translateX(-105%);
			transition: transform 0.2s ease;
			box-shadow: var(--shadow);
			background: rgba(255, 255, 255, 0.97);
			padding: 0.85rem 0.65rem 1rem !important;
		}
		:global(.shell.drawer-open) .sidebar {
			transform: translateX(0);
		}
		.collapse-btn {
			display: none;
		}
		:global(.shell.collapsed) .brand-full,
		:global(.shell.collapsed) .nav-group-label,
		:global(.shell.collapsed) .nav-label,
		:global(.shell.collapsed) .foot-full {
			display: initial;
		}
		:global(.shell.collapsed) .brand-full {
			position: static;
			width: auto;
			height: auto;
			margin: 0;
			clip: auto;
			overflow: visible;
		}
		:global(.shell.collapsed) .nav-item {
			justify-content: flex-start;
			padding: 0.45rem 0.55rem;
		}
		:global(.shell.collapsed) .foot-short {
			display: none;
		}
		:global(.shell.collapsed) .nav-group-label {
			display: block;
		}
		:global(.shell.collapsed) .nav-label {
			display: inline;
		}
	}
</style>
