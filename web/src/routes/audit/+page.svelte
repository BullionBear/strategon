<script lang="ts">
	import { onMount } from 'svelte';
	import { client } from '$lib/api';
	import type { AuditEntry } from '$lib/gen/strategyplatform/v1/control_service_pb';

	let entries = $state<AuditEntry[]>([]);
	let error = $state('');

	onMount(async () => {
		try {
			const res = await client.listAudit({ pageSize: 100 });
			entries = res.entries;
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		}
	});
</script>

<section class="fade-in">
	<h1>Audit</h1>
	<p class="muted">
		In-memory audit trail from this control-plane process. History does not survive restarts —
		Postgres persistence is a follow-up.
	</p>

	<div class="panel notice" style="margin-top:1rem">
		<strong>Limited history:</strong> entries live only in the in-memory store. Durable audit
		requires the Postgres store (ARCHITECTURE §16.3).
	</div>

	{#if error}
		<p class="pill bad" style="margin-top:1rem">{error}</p>
	{/if}

	{#if entries.length === 0}
		<p class="muted" style="margin-top:1.25rem">No audit entries yet.</p>
	{:else}
		<table>
			<thead>
				<tr>
					<th>When</th>
					<th>Action</th>
					<th>Machine</th>
					<th>Strategy</th>
					<th>From → To</th>
					<th>Actor</th>
				</tr>
			</thead>
			<tbody>
				{#each entries as e}
					<tr>
						<td class="mono">
							{e.timestamp ? new Date(Number(e.timestamp.seconds) * 1000).toISOString() : '—'}
						</td>
						<td>{e.action}</td>
						<td class="mono">{e.machineId}</td>
						<td class="mono">{e.strategy}</td>
						<td class="mono">{e.fromVersion || '—'} → {e.toVersion || '—'}</td>
						<td>{e.actor}</td>
					</tr>
				{/each}
			</tbody>
		</table>
	{/if}
</section>

<style>
	.notice {
		border-color: #e6c98a;
		background: #fff8e8;
		font-size: 0.92rem;
	}
	table {
		width: 100%;
		border-collapse: collapse;
		margin-top: 1.25rem;
		font-size: 0.9rem;
	}
	th,
	td {
		text-align: left;
		padding: 0.55rem 0.65rem;
		border-bottom: 1px solid var(--line);
	}
	th {
		font-size: 0.72rem;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--ink-muted);
	}
</style>
