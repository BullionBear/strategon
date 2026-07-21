import { DeployPhase } from '$lib/gen/strategyplatform/v1/status_pb';

/** Happy-path deploy phases in order. */
export const HAPPY_PATH: DeployPhase[] = [
	DeployPhase.PENDING,
	DeployPhase.DOWNLOADING,
	DeployPhase.VERIFYING,
	DeployPhase.DRAINING,
	DeployPhase.SWITCHING,
	DeployPhase.STARTING,
	DeployPhase.HEALTH_CHECKING,
	DeployPhase.HEALTHY
];

/** Failure branch. */
export const FAIL_PATH: DeployPhase[] = [DeployPhase.ROLLING_BACK, DeployPhase.ROLLED_BACK];

const LABELS: Record<number, string> = {
	[DeployPhase.UNSPECIFIED]: '—',
	[DeployPhase.PENDING]: 'Pending',
	[DeployPhase.DOWNLOADING]: 'Downloading',
	[DeployPhase.VERIFYING]: 'Verifying',
	[DeployPhase.DRAINING]: 'Draining',
	[DeployPhase.SWITCHING]: 'Switching',
	[DeployPhase.STARTING]: 'Starting',
	[DeployPhase.HEALTH_CHECKING]: 'Health check',
	[DeployPhase.HEALTHY]: 'Healthy',
	[DeployPhase.ROLLING_BACK]: 'Rolling back',
	[DeployPhase.ROLLED_BACK]: 'Rolled back',
	[DeployPhase.FAILED]: 'Failed',
	[DeployPhase.STOPPED]: 'Stopped'
};

export function phaseLabel(p: DeployPhase | number | undefined): string {
	return LABELS[p ?? 0] ?? String(p);
}

export function isFailPhase(p: DeployPhase | number | undefined): boolean {
	return (
		p === DeployPhase.FAILED ||
		p === DeployPhase.ROLLING_BACK ||
		p === DeployPhase.ROLLED_BACK
	);
}

export function happyIndex(p: DeployPhase | number | undefined): number {
	return HAPPY_PATH.indexOf((p ?? DeployPhase.UNSPECIFIED) as DeployPhase);
}
