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
