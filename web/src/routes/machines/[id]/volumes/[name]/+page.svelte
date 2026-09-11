<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { watchMachine } from '$lib/api';
	import type { Machine } from '$lib/gen/strategyplatform/v1/control_service_pb';
	import WorkDirBrowser from '$lib/WorkDirBrowser.svelte';

	let machine = $state<Machine | null>(null);
	let live = $state(false);

	const id = $derived(page.params.id ?? '');
	const name = $derived(page.params.name ?? '');

	onMount(() => {
		const ac = new AbortController();
		live = true;
		watchMachine(id, (m) => (machine = m), ac.signal).finally(() => (live = false));
		return () => ac.abort();
	});
</script>

<section class="fade-in">
	<p class="muted">
		<a href="/machines/{id}">← {id}</a>
	</p>
	<div class="head">
		<div>
			<h1 class="mono">{name}</h1>
			<p class="muted">Machine volume — browse files under <span class="mono">volumes/{name}</span></p>
		</div>
		{#if live}
			<span class="pill ok"><span class="live-dot"></span> live</span>
		{/if}
	</div>

	{#if machine}
		<WorkDirBrowser
			machineId={id}
			volume={name}
			reachable={machine.reachable}
			agentVersion={machine.agentVersion}
		/>
	{/if}
</section>

<style>
	.head {
		display: flex;
		justify-content: space-between;
		align-items: flex-start;
		gap: 1rem;
		margin-top: 0.35rem;
	}
</style>
