import { create } from '@bufbuild/protobuf';
import { describe, expect, it } from 'vitest';
import { ObjectMetaSchema } from '$lib/gen/strategyplatform/v1/common_pb';
import {
	NatsClusterSchema,
	NatsClusterSpecSchema,
	NatsClusterStatusSchema
} from '$lib/gen/strategyplatform/v1/nats_pb';
import { clusterGenerationLag, clusterPhaseClass, clusterStrategy, serverPhaseLabel } from './clusters';

describe('clusterPhaseClass', () => {
	it('maps controller phases', () => {
		expect(clusterPhaseClass('Ready')).toBe('ok');
		expect(clusterPhaseClass('Failed')).toBe('bad');
		expect(clusterPhaseClass('Degraded')).toBe('bad');
		expect(clusterPhaseClass('Rolling')).toBe('lag');
		expect(clusterPhaseClass('Deleting')).toBe('off');
	});
});

describe('cluster helpers', () => {
	it('detects generation lag and default strategy', () => {
		const c = create(NatsClusterSchema, {
			metadata: create(ObjectMetaSchema, { name: 'trading', generation: 2n }),
			spec: create(NatsClusterSpecSchema, {}),
			status: create(NatsClusterStatusSchema, { observedGeneration: 1n, phase: 'Rolling' })
		});
		expect(clusterGenerationLag(c)).toBe(true);
		expect(clusterStrategy(c)).toBe('nats');
	});
});

describe('serverPhaseLabel', () => {
	it('pretty-prints proto enum names', () => {
		expect(serverPhaseLabel('DEPLOY_PHASE_HEALTHY')).toBe('Healthy');
		expect(serverPhaseLabel('DEPLOY_PHASE_HEALTH_CHECKING')).toBe('Health Checking');
		expect(serverPhaseLabel('HEALTHY')).toBe('Healthy');
		expect(serverPhaseLabel('')).toBe('—');
	});
});
