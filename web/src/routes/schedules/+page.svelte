<script lang="ts">
	import { onMount } from 'svelte';
	import { client } from '$lib/api';
	import type { Machine, StrategyView } from '$lib/gen/strategyplatform/v1/control_service_pb';
	import { CronAction } from '$lib/gen/strategyplatform/v1/common_pb';

	let machines = $state<Machine[]>([]);
	let machineId = $state('');
	let strategy = $state('');
	let cronExpr = $state('0 0 * * *');
	let timezone = $state('UTC');
	let jitter = $state(30);
	let action = $state(CronAction.RESTART);
	let busy = $state(false);
	let error = $state('');
	let info = $state('');

	const strategies = $derived.by(() => {
		const m = machines.find((x) => (x.metadata?.uid || x.metadata?.name) === machineId);
		return m?.strategies ?? [];
	});

	const currentView = $derived(
		strategies.find((s) => s.strategy === strategy) ?? null
	) as StrategyView | null;

	onMount(async () => {
		await refresh();
	});

	async function refresh() {
		const res = await client.listMachines({});
		machines = res.machines;
	}

	async function save() {
		busy = true;
		error = '';
		info = '';
		try {
			const res = await client.setSchedule({
				machineId,
				strategy,
				schedules: [
					{
						name: 'primary',
						cronExpr,
						timezone,
						action,
						jitterSeconds: jitter,
						scriptRef: ''
					}
				]
			});
			info = `Schedule saved (generation ${res.generation}). Agent evaluates it locally on each tick.`;
			await refresh();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}

	function actionLabel(a: CronAction | number | undefined): string {
		switch (a) {
			case CronAction.RESTART:
				return 'RESTART';
			case CronAction.RELOAD_CONFIG:
				return 'RELOAD_CONFIG';
			case CronAction.RUN_SCRIPT:
				return 'RUN_SCRIPT';
			default:
				return String(a ?? '');
		}
	}
</script>

<section class="fade-in">
	<h1>Schedules</h1>
	<p class="muted">
		Cron lives in strategy desired state. The control plane validates and pushes; the agent
		executes locally (timezone + jitter, defer while deploy is in flight).
	</p>

	<div class="panel form" style="margin-top:1rem">
		<label>
			Machine
			<select bind:value={machineId}>
				<option value="">Select…</option>
				{#each machines as m}
					{@const mid = m.metadata?.uid || m.metadata?.name || ''}
					<option value={mid}>{mid}</option>
				{/each}
			</select>
		</label>
		<label>
			Strategy
			<select bind:value={strategy}>
				<option value="">Select…</option>
				{#each strategies as s}
					<option value={s.strategy}>{s.strategy}</option>
				{/each}
			</select>
		</label>
		<label>Cron<input bind:value={cronExpr} class="mono" /></label>
		<label>Timezone<input bind:value={timezone} /></label>
		<label>Jitter (s)<input type="number" bind:value={jitter} /></label>
		<label>
			Action
			<select bind:value={action}>
				<option value={CronAction.RESTART}>RESTART</option>
				<option value={CronAction.RELOAD_CONFIG}>RELOAD_CONFIG</option>
			</select>
		</label>
		<button class="btn" disabled={busy || !machineId || !strategy} onclick={save}>
			Save schedule
		</button>
	</div>

	{#if currentView}
		<div class="panel" style="margin-top:1rem">
			<h2 style="margin:0 0 0.5rem;font-size:1rem">Current schedules</h2>
			{#if !currentView.schedules?.length}
				<p class="muted" style="margin:0">None configured.</p>
			{:else}
				<ul class="sched-list">
					{#each currentView.schedules as s}
						<li class="mono">
							{s.name}: {s.cronExpr} ({s.timezone}) → {actionLabel(s.action)}
							{#if s.jitterSeconds}
								<span class="muted"> ±{s.jitterSeconds}s</span>
							{/if}
						</li>
					{/each}
				</ul>
			{/if}
		</div>
	{/if}

	{#if info}
		<p class="pill ok" style="margin-top:1rem;white-space:normal;height:auto;padding:0.5rem 0.75rem">
			{info}
		</p>
	{/if}
	{#if error}
		<p class="pill bad" style="margin-top:1rem">{error}</p>
	{/if}
</section>

<style>
	.form {
		display: flex;
		flex-wrap: wrap;
		gap: 0.75rem;
		align-items: flex-end;
	}
	.sched-list {
		margin: 0;
		padding-left: 1.1rem;
		font-size: 0.9rem;
	}
</style>
