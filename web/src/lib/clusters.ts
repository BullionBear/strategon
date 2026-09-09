import type { NatsCluster } from '$lib/gen/strategyplatform/v1/nats_pb';

export function clusterPhaseClass(phase: string | undefined): string {
	switch (phase) {
		case 'Ready':
			return 'ok';
		case 'Failed':
		case 'Degraded':
			return 'bad';
		case 'Deleting':
			return 'off';
		case 'Rolling':
		case 'Pending':
			return 'lag';
		default:
			return '';
	}
}

export function clusterGenerationLag(c: NatsCluster): boolean {
	const gen = c.metadata?.generation ?? 0n;
	const obs = c.status?.observedGeneration ?? 0n;
	return gen !== obs;
}

export function clusterStrategy(c: NatsCluster): string {
	return c.spec?.strategy?.trim() || 'nats';
}

/** Controller stores proto enum names (DEPLOY_PHASE_HEALTHY). */
export function serverPhaseLabel(phase: string | undefined): string {
	const raw = (phase ?? '').trim();
	if (!raw || raw === '—') return '—';
	const trimmed = raw.replace(/^DEPLOY_PHASE_/, '');
	return trimmed
		.toLowerCase()
		.split('_')
		.filter(Boolean)
		.map((w) => w.charAt(0).toUpperCase() + w.slice(1))
		.join(' ');
}
