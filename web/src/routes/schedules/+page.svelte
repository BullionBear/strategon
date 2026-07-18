<script lang="ts">
	import { onMount } from 'svelte';
	import { client } from '$lib/api';
	import type { Machine } from '$lib/gen/strategyplatform/v1/control_service_pb';
	import { CronAction } from '$lib/gen/strategyplatform/v1/common_pb';

	let machines = $state<Machine[]>([]);
	let machineId = $state('');
	let strategy = $state('');
	let cronExpr = $state('0 0 * * *');
	let timezone = $state('UTC');
	let jitter = $state(30);
	let busy = $state(false);
	let error = $state('');
	let info = $state('');

	onMount(async () => {
		const res = await client.listMachines({});
		machines = res.machines;
	});

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
						action: CronAction.RESTART,
						jitterSeconds: jitter,
						scriptRef: ''
					}
				]
			});
			info = `Schedule written to spec (generation ${res.generation}). Local cron executor is not implemented yet — agent will receive the schedule in DesiredState but will not run it until that lands.`;
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}
</script>

<section class="fade-in">
	<h1>Schedules</h1>
	<p class="muted">
		Writes cron into strategy spec via SetSchedule. The agent-side executor is still pending —
		this UI is honest about that.
	</p>

	<div class="panel notice" style="margin-top:1rem">
		<strong>Pending:</strong> cron is stored and pushed in DesiredState, but the agent does not
		execute it yet (ARCHITECTURE §10).
	</div>

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
		<label>Strategy<input bind:value={strategy} /></label>
		<label>Cron<input bind:value={cronExpr} class="mono" /></label>
		<label>Timezone<input bind:value={timezone} /></label>
		<label>Jitter (s)<input type="number" bind:value={jitter} /></label>
		<button class="btn" disabled={busy || !machineId || !strategy} onclick={save}>
			Save schedule
		</button>
	</div>

	{#if info}
		<p class="pill lag" style="margin-top:1rem;white-space:normal;height:auto;padding:0.5rem 0.75rem">
			{info}
		</p>
	{/if}
	{#if error}
		<p class="pill bad" style="margin-top:1rem">{error}</p>
	{/if}
</section>

<style>
	.notice {
		border-color: #e6c98a;
		background: #fff8e8;
		font-size: 0.92rem;
	}
	.form {
		display: flex;
		flex-wrap: wrap;
		gap: 0.75rem;
		align-items: flex-end;
	}
</style>
