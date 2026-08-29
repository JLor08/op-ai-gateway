// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { messages } from './i18n';

describe('activityNewRequests interpolation', () => {
  it('uses singular for one and plural otherwise (de)', () => {
    expect(messages.de.activityNewRequests(1)).toBe('1 neue Anfrage — aktualisieren');
    expect(messages.de.activityNewRequests(5)).toBe('5 neue Anfragen — aktualisieren');
  });

  it('uses singular for one and plural otherwise (en)', () => {
    expect(messages.en.activityNewRequests(1)).toBe('1 new request — refresh');
    expect(messages.en.activityNewRequests(3)).toBe('3 new requests — refresh');
  });
});

describe('capture-view i18n keys', () => {
  it('defines all capture keys in de and en', () => {
    const keys = [
      'activityColView',
      'captureDialogTitle',
      'captureReqHeaders',
      'captureReqBody',
      'captureRespHeaders',
      'captureRespBody',
      'captureTranslatedReqTitle',
      'captureTranslatedRespTitle',
      'captureTranslatedNote',
      'capturePretty',
      'captureRaw',
      'captureChat',
      'captureCopy',
      'captureTruncated',
      'captureHeaderValue',
      'captureHttpStatus',
      'captureCreatedAt',
      'captureClose',
      'captureSecurityNote',
      'captureDelete',
      'captureDeleteConfirm',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('capture_enabled system-setting i18n keys', () => {
  it('defines captureEnabledLabel and captureEnabledNote in de and en', () => {
    const keys = ['captureEnabledLabel', 'captureEnabledNote'] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('smtp system-setting i18n keys', () => {
  it('defines the smtp panel + invite-email keys in de and en', () => {
    const keys = [
      'smtpTitle',
      'smtpIntro',
      'smtpEnabledLabel',
      'smtpEnabledNote',
      'smtpHostLabel',
      'smtpPortLabel',
      'smtpPortError',
      'smtpUsernameLabel',
      'smtpPasswordLabel',
      'smtpPasswordSetPlaceholder',
      'smtpPasswordNote',
      'smtpPasswordClear',
      'smtpFromLabel',
      'smtpFromNameLabel',
      'smtpTlsModeLabel',
      'smtpTlsStartTls',
      'smtpTlsSsl',
      'smtpTlsNone',
      'smtpTestButton',
      'smtpTestToLabel',
      'smtpTestSuccess',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });

  it('interpolates the function-typed smtp/invite keys', () => {
    expect(messages.de.smtpTestError('x')).toBe('Testmail fehlgeschlagen: x');
    expect(messages.de.smtpTestError('')).toBe('Testmail fehlgeschlagen.');
    expect(messages.en.userInviteEmailSent('a@b.co')).toBe('Email sent to a@b.co.');
    expect(messages.de.userInviteEmailFailed('boom')).toBe(
      'E-Mail konnte nicht gesendet werden: boom',
    );
  });
});

describe('running-connections (active requests) i18n keys', () => {
  it('defines the active-panel keys in de and en', () => {
    const keys = [
      'activityActiveTitle',
      'activityActiveEmpty',
      'activityActiveElapsed',
      'activityActiveSession',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('activity time-series i18n keys', () => {
  it('defines all time-series chart/control keys in de and en', () => {
    const keys = [
      'activityTsWindowLabel',
      'activityTsBucketLabel',
      'activityChartsTitle',
      'activityChartsToggle',
      'tsUnitMin',
      'tsUnitHour',
      'tsUnitDay',
      'tsUnitDays',
      'tsUnitWeek',
      'tsUnitWeeks',
      'tsUnitMonth',
      'tsUnitMonths',
      'tsUnitYear',
      'tsUnitYears',
      'activityTsConnections',
      'activityTsConnectionsThroughput',
      'activityTsConcurrency',
      'activityTsPromptThroughput',
      'activityTsCompletionThroughput',
      'activityTsUnitReqPerSec',
      'activityTsUnitTokPerSec',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
    }
    // The labelled (non-unit) keys are always non-empty.
    expect(messages.de.activityTsWindowLabel.length).toBeGreaterThan(0);
    expect(messages.en.activityTsBucketLabel.length).toBeGreaterThan(0);
    expect(messages.de.activityTsConnections.length).toBeGreaterThan(0);
    expect(messages.en.activityTsCompletionThroughput.length).toBeGreaterThan(0);
    expect(messages.de.activityTsUnitTokPerSec.length).toBeGreaterThan(0);
  });
});

describe('server performance i18n keys', () => {
  it('defines all performance view chart/control keys in de and en', () => {
    const keys = [
      'serverPerformance',
      'serverPerfWindowLabel',
      'serverPerfLive',
      'serverPerfPaused',
      'serverPerfPauseToggle',
      'serverPerfNoAgent',
      'serverPerfGpuUtil',
      'serverPerfGpuVram',
      'serverPerfGpuVramMb',
      'serverPerfGpuTemp',
      'serverPerfGpuPower',
      'serverPerfCpu',
      'serverPerfCpuCores',
      'serverPerfCpuPower',
      'serverPerfCpuTemp',
      'serverPerfSystemPower',
      'serverPerfMem',
      'serverPerfLoad',
      'serverPerfNet',
      'serverPerfMemUsed',
      'serverPerfSwapUsed',
      'serverPerfLoad1',
      'serverPerfLoad5',
      'serverPerfLoad15',
      'serverPerfNetRx',
      'serverPerfNetTx',
      'serverPerfTokThroughput',
      'serverPerfUnitPct',
      'serverPerfUnitCelsius',
      'serverPerfUnitWatt',
      'serverPerfUnitMb',
      'serverPerfUnitBytesPerSec',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
    }
    // The function-typed stale banner renders a numeric-interpolated string.
    expect(typeof messages.de.serverPerfStale).toBe('function');
    expect(typeof messages.en.serverPerfStale).toBe('function');
    expect(messages.de.serverPerfStale(5).length).toBeGreaterThan(0);
    expect(messages.en.serverPerfStale(5)).toContain('5');
    // The labelled (non-unit) keys are always non-empty.
    expect(messages.de.serverPerformance.length).toBeGreaterThan(0);
    expect(messages.en.serverPerfGpuUtil.length).toBeGreaterThan(0);
    expect(messages.de.serverPerfNoAgent.length).toBeGreaterThan(0);
  });
});

describe('ChatSession row i18n keys', () => {
  it('defines chatSessionName and chatSessionHint in de and en', () => {
    const keys = ['chatSessionName', 'chatSessionHint'] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('chat remember-unavailable-model i18n keys', () => {
  it('defines chatModelUnavailable in de and en', () => {
    const keys = ['chatModelUnavailable'] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('activity stat-tile settings i18n keys', () => {
  it('defines the tile settings-menu keys in de and en', () => {
    const keys = ['activityTilesButton', 'activityTilesTitle'] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('agent-token copy i18n key', () => {
  it('defines agentTokenCopy in de and en', () => {
    const keys = ['agentTokenCopy'] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });

  it('defines the server-owner resource-group self-service keys in de and en', () => {
    const keys = [
      'serverResourceGroupsAction',
      'serverResourceGroupsTitle',
      'serverResourceGroupsColGroup',
      'serverResourceGroupsColMember',
      'serverResourceGroupsEmpty',
      'serverResourceGroupsJoinError',
      'serverResourceGroupsLeaveError',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('agent-binary download i18n keys', () => {
  it('defines the download section + netbird agent-download-only keys in de and en', () => {
    const keys = [
      'agentDownloadHeading',
      'agentDownloadEmpty',
      'agentDownloadButton',
      'agentDownloadVersion',
      'agentDownloadChecksum',
      'agentDownloadSelectSystem',
      'agentDownloadCopyCurl',
      'agentDownloadMeshNote',
      'agentDownloadConfig',
      'settingsNetbirdAgentDownloadOnly',
      'settingsNetbirdAgentDownloadOnlyHint',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('server override i18n keys', () => {
  it('defines the picker/force/hint + error-code keys in de and en', () => {
    const keys = [
      'serverOverrideLabel',
      'serverOverrideNote',
      'serverOverrideNone',
      'serverOverrideForceLabel',
      'serverOverrideForceHelp',
      'serverOverrideFilteredHint',
      'serverOverrideLockedHint',
      'errorServerOverrideForbidden',
      'errorServerOverrideServerUnavailable',
      'errorServerOverrideModelUnavailable',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('token rotate i18n keys', () => {
  it('defines the rotate keys in de and en', () => {
    const keys = [
      'tokenActionRotate',
      'tokenRotateConfirmTitle',
      'tokenRotateConfirmBody',
      'tokenRotateConfirm',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });

  it('uses the agreed rotate copy', () => {
    expect(messages.de.tokenActionRotate).toBe('Neu erzeugen');
    expect(messages.en.tokenActionRotate).toBe('Regenerate');
    expect(messages.de.tokenRotateConfirmTitle).toBe('Token neu erzeugen?');
    expect(messages.en.tokenRotateConfirmTitle).toBe('Regenerate token?');
    expect(messages.de.tokenRotateConfirmBody).toBe(
      'Der bisherige Token wird sofort ungültig. Ein neuer Token wird erzeugt und einmalig angezeigt.',
    );
    expect(messages.en.tokenRotateConfirmBody).toBe(
      'The current token is invalidated immediately. A new token is generated and shown once.',
    );
    expect(messages.de.tokenRotateConfirm).toBe('Neu erzeugen');
    expect(messages.en.tokenRotateConfirm).toBe('Regenerate');
  });
});

describe('activity user-column label (Besitzer -> Benutzer)', () => {
  it('labels the Activity user column as Benutzer/User, not Besitzer/Owner', () => {
    expect(messages.de.activityColOwner).toBe('Benutzer');
    expect(messages.en.activityColOwner).toBe('User');
    expect(messages.de.activityOwnerDisplayLabel).toBe('Benutzer anzeigen');
    expect(messages.en.activityOwnerDisplayLabel).toBe('User display');
  });

  it('leaves the AI-Server owner strings unchanged', () => {
    expect(messages.de.serverOwnersLabel).toBe('Besitzer');
    expect(messages.de.tableOwners).toBe('Besitzer');
    expect(messages.de.errorServerOwnerInvalid).toBe('Ungültiger Besitzer');
  });
});

describe('invite copy i18n key', () => {
  it('defines userInviteCopy in de and en with the contract values', () => {
    expect(messages.de.userInviteCopy).toBe('Einladungslink kopieren');
    expect(messages.en.userInviteCopy).toBe('Copy invite link');
  });
});

describe('activity user/token filter i18n keys', () => {
  it('defines the filter keys in de and en', () => {
    const keys = [
      'activityScopeSpecificUser',
      'activityUserFilterLabel',
      'activityTokenFilterLabel',
      'activityFilterAll',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
    expect(messages.de.activityScopeSpecificUser).toBe('Bestimmter Nutzer');
    expect(messages.en.activityScopeSpecificUser).toBe('Specific user');
  });
});

describe('activity group-by i18n keys', () => {
  it('defines the group-by keys in de and en', () => {
    const keys = [
      'activityGroupBy',
      'activityGroupAddLevel',
      'activityGroupRemoveLevel',
      'activityGroupClear',
      'activityGroupNone',
      'activityGroupSession',
      'activityGroupServer',
      'activityGroupUser',
      'activityGroupToken',
      'activityGroupModel',
      'activityGroupService',
      'activityGroupProject',
      'activityGroupCount',
      'activityGroupSpan',
      'activityGroupTokenNone',
      'activityGroupServiceNone',
      'activityGroupProjectNone',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
    expect(messages.de.activityGroupBy).toBe('Gruppieren nach');
    expect(messages.en.activityGroupBy).toBe('Group by');
    expect(messages.de.activityGroupTokenNone).toBe('(Sitzung)');
    expect(messages.en.activityGroupTokenNone).toBe('(Session)');
  });
});

describe('activity custom time-range i18n keys', () => {
  it('defines the custom-range keys in de and en', () => {
    const keys = ['activityRangeCustom', 'activityRangeFrom', 'activityRangeTo'] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
    expect(messages.de.activityRangeCustom).toBe('Benutzerdefiniert');
    expect(messages.en.activityRangeCustom).toBe('Custom');
  });
});

describe('totp 2fa i18n keys', () => {
  it('defines all totp keys in de and en', () => {
    const keys = [
      'totpTitle',
      'totpIntro',
      'totpStatusEnabled',
      'totpStatusDisabled',
      'totpEnrollButton',
      'totpEnrollScanHint',
      'totpSecretLabel',
      'totpQrAlt',
      'totpCodeLabel',
      'totpConfirmButton',
      'totpConfirmSuccess',
      'totpCopySecret',
      'totpDisableButton',
      'totpDisableTitle',
      'totpDisableHint',
      'totpDisableSuccess',
      'loginTotpTitle',
      'loginTotpIntro',
      'loginTotpEnrollTitle',
      'loginVerifyButton',
      'totpModeLabel',
      'totpModeNote',
      'totpModeOff',
      'totpModeOptional',
      'totpModeRequired',
      'settingsVisionSectionTitle',
      'settingsVisionProbeMode',
      'settingsVisionProbeModeAccept',
      'settingsVisionProbeModeVerify',
      'settingsEnergyTitle',
      'settingsEnergyPricePerKwh',
      'settingsEnergyPue',
      'settingsEnergyWhPerToken',
      'systemCurrencyFactor',
      'systemCurrencyFactorHelp',
      'userActionResetTotp',
      'userResetTotpConfirmTitle',
      'userResetTotpConfirmBody',
      'userResetTotpConfirm',
      'userResetTotpSuccess',
      'errorAuthTotpInvalid',
      'errorTotpInvalidCode',
      'errorTotpDisableForbidden',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
    expect(messages.de.totpModeRequired).toBe('Erforderlich');
    expect(messages.en.userActionResetTotp).toBe('Reset TOTP');
  });
});

describe('logs view i18n keys', () => {
  it('defines all logs view/nav keys in de and en', () => {
    const keys = [
      'logsNav',
      'logsTitle',
      'logsLevelLabel',
      'logsLevelTrace',
      'logsLevelDebug',
      'logsLevelInfo',
      'logsLevelWarn',
      'logsLevelError',
      'logsPause',
      'logsLive',
      'logsClear',
      'logsEmpty',
      'logsColTime',
      'logsColLevel',
      'logsColMsg',
      'logsTracingLabel',
      'logsTracingHelp',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
    expect(messages.de.logsLevelLabel).toBe('Loglevel');
    expect(messages.de.logsEmpty).toBe('Noch keine Logs.');
  });
});

describe('model-mapping performance-metric i18n keys', () => {
  it('defines the mapping metric form/list keys in de and en', () => {
    const keys = [
      'mappingMetricsSection',
      'mappingMetricsHint',
      'mappingContextSize',
      'mappingEnergyWhPerToken',
      'mappingGenTokensPerSecond',
      'mappingPromptTokensPerSecond',
      'mappingLoadTimeMs',
      'mappingIsMtp',
      'mappingVisionCapable',
      'mappingMetricsLocked',
      'mappingMaxConcurrency',
      'mappingRecommendedConcurrency',
      'mappingGenTpsAtCapacity',
      'mappingAppNameReadOnly',
      'mappingProbeContext',
      'mappingProbeContextRunning',
      'mappingProbeContextFailed',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
    expect(messages.de.mappingContextSize).toBe('Kontextgröße');
    expect(messages.en.mappingContextSize).toBe('Context size');
  });
});

describe('admission-queue timeout i18n keys', () => {
  it('defines the admission-queue timeout field keys in de and en', () => {
    const keys = [
      'applicationAdmissionQueueTimeout',
      'applicationAdmissionQueueTimeoutHelp',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('benchmark i18n keys', () => {
  it('defines the benchmark button/status + error-code keys in de and en', () => {
    const keys = [
      'runBenchmark',
      'benchmarkAll',
      'benchmarking',
      'benchmarkDone',
      'benchmarkFailed',
      'benchmarkLive',
      'benchmarkProgress',
      'benchmarkHistory',
      'benchmarkHistoryEmpty',
      'benchmarkRunAt',
      'errorBenchmarkAlreadyRunning',
      'errorBenchmarkServerInUse',
      'errorBenchmarkNoModels',
      'benchmarkArea',
      'benchmarkScope',
      'benchmarkScopeServer',
      'benchmarkScopeApplication',
      'benchmarkScopeMapping',
      'benchmarkType',
      'benchmarkTypeSpeed',
      'benchmarkTypeCapacity',
      'benchmarkTypeBoth',
      'benchmarkTypeVision',
      'benchmarkStart',
      'benchmarkServerBusy',
      'benchmarkRunning',
      'benchmarkLastCompleted',
      'modelServerTitle',
      'modelServerColServer',
      'modelServerColPrio',
      'modelServerColModel',
      'groupServersIntro',
      'modelServerNotLoaded',
      'modelServerSource',
      'modelServerUpdated',
      'modelServerLoad',
      'modelServerLoadDisabledPerm',
      'modelServerLoadDisabledLoaded',
      'modelServerLoadDisabledBusy',
      'modelServerLoadStarted',
      'modelServerLoadSuccess',
      'modelServerLoadError',
      'modelServerBusy',
      'modelServerAlreadyRunning',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
    expect(messages.de.benchmarkAll).toBe('Alle benchmarken');
    expect(messages.en.benchmarkAll).toBe('Benchmark all');
  });
});

describe('capacity-benchmark i18n keys', () => {
  it('defines all capacity keys in de and en', () => {
    const keys = [
      'runCapacityBenchmark',
      'benchmarkCapacityAll',
      'benchmarkCurrentConcurrency',
      'benchmarkMaxConcurrency',
      'benchmarkRecommendedConcurrency',
      'benchmarkGenTpsAtCapacity',
      'benchmarkVision',
      'benchmarkCapacityRuns',
      'benchmarkVisionRuns',
      'benchmarkMemoryObserved',
      'benchmarkColConcurrency',
      'benchmarkColAggregateTps',
      'benchmarkColLatency',
      'benchmarkColErrors',
      'benchmarkLevelStop',
    ] as const;
    for (const k of keys) {
      expect(messages.de[k]).toBeTruthy();
      expect(messages.en[k]).toBeTruthy();
    }
  });
});

describe('loaded-model i18n keys', () => {
  it('defines the loaded-model table/chat/application keys in de and en', () => {
    const keys = [
      'tableModelLoaded',
      'tableModelOffered',
      'tableModelVision',
      'tableModelType',
      'chatModelLoaded',
      'chatModelLoadedOn',
      'chatImageModelUnsupported',
      'applicationLoadedModelsLegend',
      'applicationLoadedModelsPath',
      'applicationLoadedModelsFormat',
      'applicationLoadedModelsNote',
      'applicationLoadedFormatAuto',
      'applicationLoadedFormatOpenai',
      'applicationLoadedFormatLlamaSwap',
      'applicationLoadedFormatLlamaCpp',
      'applicationLoadedFormatLitellm',
      'applicationContextProbePath',
      'applicationContextProbePathHelp',
      'applicationContextProbeNote',
      'applicationMetricsLegend',
      'applicationScheduledBenchmark',
      'applicationScheduledBenchmarkIntervalLabel',
      'applicationOpportunisticMetrics',
      'applicationMetricsNote',
      'serverPathSuffixLabel',
      'serverPathSuffixHelp',
      'serverAdminGroupLabel',
      'serverAdminGroupSystemGroupLabel',
      'serverNoAdminGroupHint',
      'serverAdminGroupsSectionTitle',
      'serverAdminGroupsSave',
      'serverAdminGroupsSaved',
      'serverEnergySection',
      'serverEnergySectionIntro',
      'serverEnergySave',
      'serverEnergySaved',
      'serverEstimatedWatts',
      'serverEstimatedWattsHelp',
      'serverIdleWatts',
      'serverIdleWattsHelp',
      'serverPricePerKwh',
      'priceUnitLabel',
      'serverPue',
      'serverPueHelp',
      'applicationPathSuffixLabel',
      'applicationApiTokenLabel',
      'applicationApiTokenNote',
      'applicationApiTokenSetPlaceholder',
      'applicationApiTokenClear',
      'applicationApiTokenHeaderLabel',
      'applicationApiTokenHeaderHelp',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
    expect(messages.de.tableModelLoaded).toBe('Geladen');
    expect(messages.en.tableModelLoaded).toBe('Loaded');
    // Both admin-group auto-note keys (Phase B, spec 2026-08-10) are
    // interpolating functions (like serverNetbirdPeerDomainHint), not plain
    // strings.
    expect(typeof messages.de.serverAdminGroupAuto).toBe('function');
    expect(typeof messages.en.serverAdminGroupAuto).toBe('function');
    expect(messages.de.serverAdminGroupAuto('Alpha')).toContain('Alpha');
    expect(messages.en.serverAdminGroupAuto('Alpha')).toContain('Alpha');
    expect(typeof messages.de.serverAdminGroupSystemGroupAuto).toBe('function');
    expect(typeof messages.en.serverAdminGroupSystemGroupAuto).toBe('function');
    expect(messages.de.serverAdminGroupSystemGroupAuto('Beta')).toContain('Beta');
    expect(messages.en.serverAdminGroupSystemGroupAuto('Beta')).toContain('Beta');
  });
});

describe('netbird enhancement i18n keys', () => {
  it('defines the enroll + linkage-editor keys in de and en', () => {
    const keys = [
      'serverNetbirdEnroll',
      'serverNetbirdLinkTitle',
      'serverNetbirdEnabledLabel',
      'serverNetbirdPeerId',
      'serverNetbirdGroupId',
      'serverNetbirdGroups',
      'serverNetbirdGroupsUnenrolled',
      'serverNetbirdSetupKeyId',
      'serverNetbirdLinkSave',
      'serverNetbirdLinkSaved',
      'serverNetbirdPeerPick',
      'serverNetbirdPeerInUse',
      'serverNetbirdPeerLinked',
      'serverNetbirdDeletePeer',
      'serverNetbirdPeerDeleteWarning',
      'serverNetbirdSetupCommand',
      'serverNetbirdPeerManagedLabel',
      'serverNetbirdPeerManagedHelp',
      'serverNetbirdPeerNotManaged',
      'serverNetbirdPolicyExclude',
      'serverNetbirdPolicyExcludeHelp',
      'serverNetbirdPolicyInclude',
      'serverNetbirdPolicyIncludeHelp',
      'serverNetbirdPolicyForcedNote',
      'serverNetbirdPolicyPrecheckNote',
      'serverNetbirdPolicyOptOutDenyNote',
      'serverNetbirdOnlyPolicyWarning',
      'serverNetbirdOnlyForcedNote',
      'serverNetbirdOnlyPrecheckWarning',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
    expect(messages.de.serverNetbirdLinkTitle).toBe('NetBird-Verknüpfung');
    expect(messages.en.serverNetbirdEnroll).toBe('NetBird enrollment (setup key)');
    // The domain hint is an interpolating function (like serverNetbirdError).
    expect(typeof messages.de.serverNetbirdPeerDomainHint).toBe('function');
    expect(typeof messages.en.serverNetbirdPeerDomainHint).toBe('function');
    expect(messages.de.serverNetbirdPeerDomainHint('host.netbird.io')).toContain('host.netbird.io');
    expect(messages.en.serverNetbirdPeerDomainHint('host.netbird.io')).toContain('host.netbird.io');
  });
});

describe('netbird-only transport settings i18n keys', () => {
  it('defines the netbird_only toggle + gateway-peer + status keys in de and en', () => {
    const keys = [
      'settingsNetbirdOnly',
      'settingsNetbirdOnlyHelp',
      'settingsNetbirdGatewayPeer',
      'settingsNetbirdGatewayPeerRestartHelp',
      'settingsNetbirdGatewayPeerName',
      'settingsNetbirdGatewayPeerNameHelp',
      'settingsNetbirdOnlyNoListenerWarning',
      'settingsNetbirdStatusTitle',
      'settingsNetbirdListenerInactive',
      'settingsNetbirdGatewayPeerConnected',
      'settingsNetbirdGatewayPeerDisconnected',
      'settingsNetbirdGatewayKeyCreate',
      'settingsNetbirdGatewayKeyTitle',
      'settingsNetbirdGatewayKeyHelp',
      'settingsNetbirdSidecarEnroll',
      'settingsNetbirdSidecarEnrolled',
      'settingsNetbirdSidecarNoKeyFile',
      'settingsNetbirdReenrollConfirmTitle',
      'settingsNetbirdReenrollConfirmBody',
      'settingsNetbirdReenrollConfirmAction',
      'settingsNetbirdGatewayPeerChangeConfirmTitle',
      'settingsNetbirdGatewayPeerChangeConfirmBody',
      'settingsNetbirdGatewayPeerChangeConfirmAction',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
    // The listener-active label interpolates the bind address (like serverNetbirdError).
    expect(typeof messages.de.settingsNetbirdListenerActive).toBe('function');
    expect(typeof messages.en.settingsNetbirdListenerActive).toBe('function');
    expect(messages.de.settingsNetbirdListenerActive('100.1.2.3:8081')).toContain('100.1.2.3:8081');
    expect(messages.en.settingsNetbirdListenerActive('100.1.2.3:8081')).toContain('100.1.2.3:8081');
  });
});

describe('model-groups + visibility i18n keys', () => {
  it('defines the group-management + model-visibility keys in de and en', () => {
    const keys = [
      'modelGroups',
      'modelGroupCreate',
      'modelGroupEditTitle',
      'modelGroupGatewayName',
      'modelGroupDisplayName',
      'modelGroupStatus',
      'modelGroupMode',
      'modelGroupModeSticky',
      'modelGroupModeClimb',
      'modelGroupModeHelp',
      'modelGroupMembers',
      'modelGroupAddMember',
      'modelGroupMemberCount',
      'modelGroupEdit',
      'modelGroupDelete',
      'modelGroupDeleteConfirm',
      'modelGroupSave',
      'modelGroupCancel',
      'modelGroupEmpty',
      'modelGroupNameConflict',
      'modelGroupMoveUp',
      'modelGroupMoveDown',
      'modelGroupRemoveMember',
      'modelGroupChip',
      'modelGroupTraversal',
      'modelGroupTraversalDepth',
      'modelGroupTraversalBreadth',
      'modelGroupTraversalRoundRobin',
      'modelGroupTraversalHelp',
      'modelGroupCycleError',
      'modelGroupLoadedOnlyHelp',
      'modelGroupMemberOrderHelp',
      'modelGroupClimbSpeedMarginHelp',
      'modelGroupMinTokensPerSecondHelp',
      'modelGroupMinSpeedFallbackHelp',
      'modelVisibility',
      'modelVisibilityShown',
      'modelVisibilityHidden',
      'modelVisibilityLocked',
      'modelVisibilityHelp',
      'modelsIntro',
      'modelsEmpty',
      'modelDetailsAction',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
    expect(messages.de.modelGroups).toBe('Modellgruppen');
    expect(messages.en.modelGroups).toBe('Model groups');
    expect(messages.de.modelVisibilityLocked).toBe('Gesperrt');
    expect(messages.en.modelVisibilityLocked).toBe('Locked');
  });
});

describe('netbird policy management settings i18n keys', () => {
  it('defines the manage-policies + scope + deny-by-default + interval keys in de and en', () => {
    const keys = [
      'settingsNetbirdManagePolicies',
      'settingsNetbirdManagePoliciesHelp',
      'settingsNetbirdPolicyScope',
      'settingsNetbirdPolicyScopeAuto',
      'settingsNetbirdPolicyScopeAll',
      'settingsNetbirdPolicyScopeSelected',
      'settingsNetbirdDenyByDefault',
      'settingsNetbirdDenyByDefaultHelp',
      'settingsNetbirdDenyByDefaultWarn',
      'settingsNetbirdDenyEnforce',
      'settingsNetbirdDenyEnforceHelp',
      'settingsNetbirdPeerInterval',
      'settingsNetbirdPeerIntervalHelp',
      'settingsNetbirdReconcileInterval',
      'settingsNetbirdReconcileIntervalHelp',
      'settingsNetbirdIntervalError',
      'settingsNetbirdIntervalOrder',
      'settingsNetbirdPolicyCouplingWarn',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
    // The effective-scope hint is an interpolating function (like serverNetbirdError).
    expect(typeof messages.de.settingsNetbirdPolicyScopeEffective).toBe('function');
    expect(typeof messages.en.settingsNetbirdPolicyScopeEffective).toBe('function');
    expect(messages.de.settingsNetbirdPolicyScopeEffective('Alle')).toContain('Alle');
    expect(messages.en.settingsNetbirdPolicyScopeEffective('All')).toContain('All');
  });
});

describe('netbird ping-allow settings + ping action i18n keys', () => {
  it('defines the ping-allow toggles + per-server label + ping action keys in de and en', () => {
    const keys = [
      'settingsNetbirdAllowPingGateway',
      'settingsNetbirdAllowPingGatewayHelp',
      'settingsNetbirdAllowPingAllServers',
      'settingsNetbirdAllowPingAllServersHelp',
      'serverNetbirdAllowPing',
      'serverNetbirdPingExclude',
      'settingsPingTitle',
      'settingsPingServerLabel',
      'settingsPingButton',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
    // The ping result strings are interpolating functions (like settingsNetbirdTestFailed).
    expect(typeof messages.de.settingsPingOk).toBe('function');
    expect(typeof messages.en.settingsPingOk).toBe('function');
    expect(messages.de.settingsPingOk(12)).toContain('12');
    expect(messages.en.settingsPingOk(12)).toContain('12');
    expect(typeof messages.de.settingsPingFailed).toBe('function');
    expect(typeof messages.en.settingsPingFailed).toBe('function');
    expect(messages.de.settingsPingFailed('boom')).toContain('boom');
    expect(messages.en.settingsPingFailed('boom')).toContain('boom');
  });
});

describe('netbird token-rotation i18n keys', () => {
  it('defines the token validity + rotate-button + threshold-field keys in de and en', () => {
    const keys = [
      'settingsNetbirdTokenValidUnknown',
      'settingsNetbirdTokenRotateBefore',
      'settingsNetbirdTokenRotateBeforeHelp',
      'settingsNetbirdRotate',
      'settingsNetbirdRotateConfirmTitle',
      'settingsNetbirdRotateConfirmBody',
      'settingsNetbirdRotateConfirmAction',
      'settingsNetbirdRotateOldDeleted',
      'settingsNetbirdRotateOldUnknown',
      'settingsNetbirdRotateFailed',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
    // The validity line + rotation-result strings interpolate the expiry date +
    // remaining days (like settingsNetbirdListenerActive).
    expect(typeof messages.de.settingsNetbirdTokenValid).toBe('function');
    expect(typeof messages.en.settingsNetbirdTokenValid).toBe('function');
    expect(messages.de.settingsNetbirdTokenValid('2027-01-01', 30)).toContain('2027-01-01');
    expect(messages.de.settingsNetbirdTokenValid('2027-01-01', 30)).toContain('30');
    expect(messages.en.settingsNetbirdTokenValid('2027-01-01', 30)).toContain('2027-01-01');
    expect(messages.en.settingsNetbirdTokenValid('2027-01-01', 30)).toContain('30');
    expect(typeof messages.de.settingsNetbirdRotateOk).toBe('function');
    expect(typeof messages.en.settingsNetbirdRotateOk).toBe('function');
    expect(messages.de.settingsNetbirdRotateOk('2027-01-01', 30)).toContain('2027-01-01');
    expect(messages.en.settingsNetbirdRotateOk('2027-01-01', 30)).toContain('30');
  });

  it('defines the NetBird-settings restructure keys (sections + Network panel) in de and en', () => {
    const keys = [
      'settingsNetbirdSectionAdmin',
      'settingsNetbirdSectionNetwork',
      'settingsNetbirdSectionPeer',
      'settingsNetbirdSectionPolicies',
      'settingsNetbirdSectionNetworkIntro',
      'settingsNetbirdDnsDomain',
      'settingsNetbirdNetworkRange',
      'settingsNetbirdNetworkRangeV6',
      'settingsNetbirdIPv6Groups',
      'settingsNetbirdIPv6GroupsHelp',
      'settingsNetbirdNetworkSaved',
      'settingsNetbirdNetworkRangeInvalid',
      'settingsNetbirdNetworkSaveConfirmTitle',
      'settingsNetbirdNetworkSaveConfirmBody',
      'settingsNetbirdNetworkSaveConfirmAction',
      'settingsNetbirdTestSaveConfirmTitle',
      'settingsNetbirdTestSaveConfirmBody',
      'settingsNetbirdTestSaveConfirmAction',
      'settingsNetbirdAdminRequired',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('server availability i18n keys', () => {
  it('defines all availability view keys in de and en', () => {
    const keys = [
      'serverAvailability',
      'availabilityHealthTimeline',
      'availabilityAgentTimeline',
      'availabilityUptimeChart',
      'availabilityAgentChart',
      'availabilityNoData',
      'availabilityStateHealthy',
      'availabilityStateDegraded',
      'availabilityStateUnhealthy',
      'availabilityStateUnknown',
      'availabilityStatePresent',
      'availabilityStateAbsent',
      'availabilityNetbirdTimeline',
      'availabilityNetbirdChart',
      'availabilityStateConnected',
      'availabilityStateDisconnected',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
    }
    expect(typeof messages.de.availabilityUptimeSummary).toBe('function');
    expect(messages.en.availabilityUptimeSummary('99.4 %')).toContain('99.4');
    expect(typeof messages.de.availabilityNetbirdSummary).toBe('function');
    expect(messages.en.availabilityNetbirdSummary('88.8 %')).toContain('88.8');
  });
});

describe('server hardware i18n keys', () => {
  it('defines all hardware view keys in de and en', () => {
    const keys = [
      'serverHardware',
      'hardwareNoReport',
      'hardwareCollectedAt',
      'hardwareSystem',
      'hardwareCpu',
      'hardwareMemory',
      'hardwareMainboard',
      'hardwareBios',
      'hardwareGpus',
      'hardwareOs',
      'hardwareKernel',
      'hardwareArch',
      'hardwareHostname',
      'hardwareAgentVersion',
      'hardwareCpuModel',
      'hardwareCpuVendor',
      'hardwareCpuCores',
      'hardwareCpuThreads',
      'hardwareBaseClock',
      'hardwareRamTotal',
      'hardwareNoModules',
      'hardwareDimmLocator',
      'hardwareDimmSize',
      'hardwareDimmType',
      'hardwareDimmSpeed',
      'hardwareVendor',
      'hardwareProduct',
      'hardwareVersion',
      'hardwareGpuIndex',
      'hardwareGpuName',
      'hardwareGpuVram',
      'hardwareGpuDriver',
      'hardwareGpuUuid',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
    }
  });
});

describe('agent-presence-timeout + Agent column i18n keys', () => {
  it('defines the column + status + system-field + per-server-field keys in de and en', () => {
    const keys = [
      'tableAgent',
      'agentStatusActive',
      'agentStatusInactive',
      'agentStatusUnconfigured',
      'settingsAgentPresenceTimeoutLabel',
      'settingsAgentPresenceTimeoutNote',
      'settingsAgentPresenceTimeoutError',
      'serverAgentPresenceTimeoutLabel',
      'serverAgentPresenceTimeoutDefault',
      'serverAgentPresenceTimeoutCustom',
      'serverAgentPresenceTimeoutSecondsLabel',
      'serverAgentPresenceTimeoutNote',
      'serverAgentPresenceTimeoutCurrent',
      'serverAgentPresenceTimeoutSaved',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
    expect(messages.de.tableAgent).toBe('Agent');
    expect(messages.de.agentStatusActive).toBe('Aktiv');
    expect(messages.de.agentStatusInactive).toBe('Inaktiv');
    expect(messages.de.agentStatusUnconfigured).toBe('Kein Agent');
    expect(messages.en.agentStatusUnconfigured).toBe('No agent');
  });
});

describe('activity session-id i18n keys', () => {
  it('defines the session + agent column keys in de and en', () => {
    const keys = ['activityColSession', 'activityColAgent'] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('activity service dimension i18n keys (Phase 1 service accounts)', () => {
  it('defines the service column + group-by-empty-key keys in de and en', () => {
    const keys = ['activityColService', 'activityGroupServiceNone'] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
    expect(messages.de.activityColService).toBe('Dienst');
    expect(messages.en.activityColService).toBe('Service');
  });
});

describe('route affinity + chat-id i18n keys', () => {
  it('defines the affinity-mode setting + copy-chat-id keys in de and en', () => {
    const keys = [
      'chatCopyId',
      'settingsRouteAffinityTitle',
      'settingsRouteAffinityModeLabel',
      'settingsRouteAffinityModeNote',
      'settingsRouteAffinityModeClient',
      'settingsRouteAffinityModeLegacy',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('energy-attribution Activity column i18n keys', () => {
  it('defines the energy column keys in de and en', () => {
    const keys = [
      'activityColEnergyWh',
      'activityColEnergyMarginalWh',
      'activityColEnergySource',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('energy attribution P3 T2 (cost column + tiles + chart) i18n keys', () => {
  it('defines the cost column + energy/cost tile + energy chart keys in de and en', () => {
    const keys = [
      'activityColCostEur',
      'activityEnergyTile',
      'activityCostTile',
      'activityEnergyChart',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
    expect(messages.de.activityColCostEur).toBe('Kosten');
    expect(messages.en.activityColCostEur).toBe('Cost');
  });
});

describe('currency unit selector i18n keys', () => {
  it('defines the cost-unit selector + currency-unit option labels in de and en', () => {
    const keys = [
      'activityCostUnit',
      'currencyUnitEur',
      'currencyUnitEurCent',
      'currencyUnitUsd',
      'currencyUnitUsdCent',
      'priceUnitLabel',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
    expect(messages.de.currencyUnitEur).toBe('Euro (€)');
    expect(messages.en.currencyUnitEur).toBe('Euro (€)');
  });
});

describe('Service Accounts (Phase 1) i18n keys', () => {
  it('defines the services nav/list/detail/token-management keys in de and en', () => {
    const keys = [
      'services',
      'servicesIntro',
      'serviceListTitle',
      'serviceCreate',
      'serviceColTokenCount',
      'serviceColDelegates',
      'serviceNameLabel',
      'serviceDescriptionLabel',
      'serviceStatusLabel',
      'serviceAllowedModelsLabel',
      'serviceAllowedModelsHelp',
      'serviceDelegatesLabel',
      'serviceDelegatesFullGroup',
      'serviceDelegatesFullHelp',
      'serviceDelegatesTokenGroup',
      'serviceDelegatesTokenHelp',
      'serviceDelegatesAddLabel',
      'serviceDelegatesAdd',
      'serviceDelegatesEmpty',
      'serviceDelegatesRemove',
      'serviceSettingsTitle',
      'serviceSettingsReadOnlyNote',
      'serviceActionDelete',
      'serviceDeleteConfirm',
      'serviceTokensTitle',
      'serviceTokensIntro',
      'serviceTokenCreate',
      'serviceTokenColPrefix',
      'serviceTokenColExpires',
      'serviceTokenColLastUsed',
      'serviceTokenCurlLabel',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
    // de/en asymmetric on purpose (per the design's naming decision — "Delegierte",
    // not "Eigentümer"/"Owner", since a service is delegated, not owned).
    expect(messages.de.serviceDelegatesLabel).toBe('Delegierte');
    expect(messages.en.serviceDelegatesLabel).toBe('Delegates');
  });
});

describe('Principal Limits (Phase 2) i18n keys', () => {
  it('defines the shared LimitsEditor + user-limits + error-code keys in de and en', () => {
    const keys = [
      'limitsTitle',
      'limitsSubtitle',
      'limitRateTitle',
      'limitRateRequestsLabel',
      'limitRateWindowLabel',
      'limitRequestQuotaTitle',
      'limitRequestQuotaLabel',
      'limitTokenQuotaTitle',
      'limitTokenQuotaLabel',
      'limitCostBudgetTitle',
      'limitCostBudgetLabel',
      'limitPeriodLabel',
      'limitPeriodOff',
      'limitPeriodHour',
      'limitPeriodDay',
      'limitPeriodWeek',
      'limitPeriodMonth',
      'errorLimitValidationFailed',
      'errorLimitUserNotFound',
      'userActionLimits',
      'userLimitsDialogSubtitle',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });

  it('defines the usage-line functions in de and en', () => {
    expect(messages.de.limitUsageRequestsLine(8000, 10000)).toContain('8000');
    expect(messages.de.limitUsageRequestsLine(8000, 10000)).toContain('10000');
    expect(messages.en.limitUsageRequestsLine(8000, 10000)).toContain('8000');
    expect(messages.de.limitUsageTokensLine(1, 2).length).toBeGreaterThan(0);
    expect(messages.en.limitUsageTokensLine(1, 2).length).toBeGreaterThan(0);
    expect(messages.de.limitUsageCostLine(1, 2).length).toBeGreaterThan(0);
    expect(messages.en.limitUsageCostLine(1, 2).length).toBeGreaterThan(0);
  });
});

describe('groups i18n keys', () => {
  it('defines the nav/section/column/action/hint/confirm string keys in de and en', () => {
    const keys = [
      'groups',
      'groupsIntro',
      'groupsSystemTitle',
      'groupsAdminTitle',
      'groupsUserTitle',
      'groupsInvitationsTitle',
      'groupsInvitationsEmpty',
      'groupsInvitedByLabel',
      'groupsColParent',
      'groupsColOwner',
      'groupsColMembers',
      'groupsColManagers',
      'groupsColMyRole',
      'groupsOwnerSelf',
      'groupsRoleOwner',
      'groupsRoleManager',
      'groupsRoleMember',
      'groupsPermUsers',
      'groupsPermGroup',
      'groupsPermServers',
      'groupsPermServices',
      'groupsPermResources',
      'groupsActionRename',
      'groupsActionMembers',
      'groupsActionDelete',
      'groupsActionLeave',
      'groupsActionAccept',
      'groupsActionDecline',
      'groupsActionAdd',
      'groupsActionInvite',
      'groupsActionPromote',
      'groupsActionDemote',
      'groupsActionRemoveMember',
      'groupsActionTransfer',
      'groupsCreateSystemTitle',
      'groupsCreateAdminTitle',
      'groupsCreateUserTitle',
      'groupsEditTitle',
      'groupsParentLabel',
      'groupsNoSystemGroupHint',
      'groupsNoAdminGroupHint',
      'groupsOwnerLabel',
      'groupsOwnerNoSystemGroupHint',
      'groupsAddMembersLabel',
      'groupsAddMembersHelp',
      'groupsRosterLabel',
      'groupsRosterEmpty',
      'groupsMemberStateInvited',
      'groupsManageLabel',
      'groupsPromoteSelectLabel',
      'groupsDemoteSelectLabel',
      'groupsTransferSelectLabel',
      'groupsDeleteConfirmTitle',
      'groupsDeleteConfirmBody',
      'groupsLeaveConfirmTitle',
      'groupsLeaveConfirmBody',
      'groupsTransferConfirmTitle',
      'groupsTransferConfirmBody',
      'errorGroupNotFound',
      'errorGroupNameConflict',
      'errorGroupNameInvalid',
      'errorGroupParentInvalid',
      'errorGroupTierInvalid',
      'errorGroupMemberNotVisible',
      'errorGroupNotParentMember',
      'errorGroupCandidateInvalid',
      'errorGroupForbidden',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });

  it('defines the name-interpolating functions in de and en', () => {
    expect(messages.de.groupsMembersTitle('Team Alpha')).toContain('Team Alpha');
    expect(messages.en.groupsMembersTitle('Team Alpha')).toContain('Team Alpha');
    expect(messages.de.groupsParentAuto('System Foo')).toContain('System Foo');
    expect(messages.en.groupsParentAuto('System Foo')).toContain('System Foo');
  });

  it('defines the group-delete coupled-projects hint function in de and en', () => {
    expect(messages.de.groupsDeleteCoupledHint('Alpha, Beta')).toContain('Alpha, Beta');
    expect(messages.en.groupsDeleteCoupledHint('Alpha, Beta')).toContain('Alpha, Beta');
  });

  it('defines the invite-form system-group picker keys in de and en (Task 15)', () => {
    const keys = [
      'userInviteAdminGroupLabel',
      'userInviteAdminGroupSelect',
      'userInviteNoAdminGroupHint',
      'errorUserNoSystemGroup',
      'errorUserSystemGroupRequired',
      'errorUserSystemGroupInvalid',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('service<->admin-group linkage i18n keys (Phase C, spec 2026-08-10)', () => {
  it('defines the create-form picker + edit-form admin-groups editor string keys in de and en', () => {
    const keys = [
      'serviceAdminGroupLabel',
      'serviceAdminGroupSystemGroupLabel',
      'serviceNoAdminGroupHint',
      'serviceAdminGroupsSectionTitle',
      'serviceAdminGroupsSave',
      'serviceAdminGroupsSaved',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });

  // Both admin-group auto-note keys (mirrors the server* linkage keys) are
  // interpolating functions, not plain strings.
  it('defines the name-interpolating functions in de and en', () => {
    expect(typeof messages.de.serviceAdminGroupAuto).toBe('function');
    expect(typeof messages.en.serviceAdminGroupAuto).toBe('function');
    expect(messages.de.serviceAdminGroupAuto('Alpha')).toContain('Alpha');
    expect(messages.en.serviceAdminGroupAuto('Alpha')).toContain('Alpha');
    expect(typeof messages.de.serviceAdminGroupSystemGroupAuto).toBe('function');
    expect(typeof messages.en.serviceAdminGroupSystemGroupAuto).toBe('function');
    expect(messages.de.serviceAdminGroupSystemGroupAuto('Beta')).toContain('Beta');
    expect(messages.en.serviceAdminGroupSystemGroupAuto('Beta')).toContain('Beta');
  });
});

describe('resource groups i18n keys (Phase 1, spec 2026-08-11)', () => {
  it('defines the nav/list/create/detail/admin-group-linkage/server-editor string keys in de and en', () => {
    const keys = [
      'resourceGroups',
      'resourceGroupsIntro',
      'resourceGroupListTitle',
      'resourceGroupCreate',
      'resourceGroupNameLabel',
      'resourceGroupStatusLabel',
      'resourceGroupColSystemGroup',
      'resourceGroupColAdminGroups',
      'resourceGroupColServers',
      'resourceGroupSettingsTitle',
      'resourceGroupActionDelete',
      'resourceGroupDeleteConfirm',
      'resourceGroupAdminGroupLabel',
      'resourceGroupAdminGroupSystemGroupLabel',
      'resourceGroupNoAdminGroupHint',
      'resourceGroupAdminGroupsSectionTitle',
      'resourceGroupAdminGroupsSave',
      'resourceGroupAdminGroupsSaved',
      'resourceGroupServersSectionTitle',
      'resourceGroupServersLabel',
      'resourceGroupServersSave',
      'resourceGroupServersSaved',
      'resourceGroupProvisionsSectionTitle',
      'resourceGroupProvisionsColKind',
      'resourceGroupProvisionsColTarget',
      'resourceGroupProvisionsEmpty',
      'resourceGroupProvisionsAddKindLabel',
      'resourceGroupProvisionsAddTargetLabel',
      'resourceGroupProvisionsAddAction',
      'resourceGroupProvisionsRemoveAction',
      'resourceGroupProvisionsSave',
      'resourceGroupProvisionsSaved',
      'resourceGroupProvisionsNoTargetsHint',
      'resourceGroupProvisionKindUser',
      'resourceGroupProvisionKindUserGroup',
      'resourceGroupProvisionKindAdminGroup',
      'resourceGroupProvisionKindService',
      'errorResourceGroupProvisionTargetInvalid',
      'settingsResourceProvisioningEnforceLabel',
      'settingsResourceProvisioningEnforceHelp',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });

  // Both admin-group auto-note keys (mirrors the server*/service* linkage
  // keys) are interpolating functions, not plain strings.
  it('defines the name-interpolating functions in de and en', () => {
    expect(typeof messages.de.resourceGroupAdminGroupAuto).toBe('function');
    expect(typeof messages.en.resourceGroupAdminGroupAuto).toBe('function');
    expect(messages.de.resourceGroupAdminGroupAuto('Alpha')).toContain('Alpha');
    expect(messages.en.resourceGroupAdminGroupAuto('Alpha')).toContain('Alpha');
    expect(typeof messages.de.resourceGroupAdminGroupSystemGroupAuto).toBe('function');
    expect(typeof messages.en.resourceGroupAdminGroupSystemGroupAuto).toBe('function');
    expect(messages.de.resourceGroupAdminGroupSystemGroupAuto('Beta')).toContain('Beta');
    expect(messages.en.resourceGroupAdminGroupSystemGroupAuto('Beta')).toContain('Beta');
  });
});

describe('projects i18n keys', () => {
  it('defines the nav/section/column/action/confirm/error string keys in de and en', () => {
    const keys = [
      'projects',
      'projectsIntro',
      'projectName',
      'projectDescription',
      'projectsColOwner',
      'projectsColMyRole',
      'projectsColMembers',
      'projectsColGroups',
      'projectsColTotalTokens',
      'projectsOwnerSelf',
      'projectsRoleOwner',
      'projectsRoleMember',
      'projectsRoleNone',
      'projectsActionRename',
      'projectsActionMembers',
      'projectsActionTokens',
      'projectsActionDelete',
      'projectsActionTransfer',
      'projectsActionAdd',
      'projectsActionRemoveMember',
      'projectsActionRemoveGroup',
      'projectsCreateTitle',
      'projectsEditTitle',
      'projectsMembersLabel',
      'projectsMembersEmpty',
      'projectsGroupsLabel',
      'projectsGroupsEmpty',
      'projectsAddMembersLabel',
      'projectsAddGroupsLabel',
      'projectsTransferLabel',
      'projectsTransferSelectLabel',
      'projectsDeleteConfirmTitle',
      'projectsDeleteConfirmBody',
      'projectsTransferConfirmTitle',
      'projectsTransferConfirmBody',
      'errorProjectNotFound',
      'errorProjectNameConflict',
      'errorProjectTransferNotMember',
      'errorProjectMemberNotVisible',
      'errorProjectGroupNotVisible',
      'errorProjectForbidden',
      'projectsCoupleToggle',
      'projectsCoupleSelectLabel',
      'projectsCoupleNewName',
      'projectsCoupleModeSelect',
      'projectsCoupleModeCreate',
      'projectsCoupledNote',
      'errorProjectCoupled',
      'errorProjectCoupleGroupInvalid',
      'errorProjectCoupleAmbiguous',
      'projectsTokensLabel',
      'projectsTokensEmpty',
      'projectsTokenOwner',
      'projectsTokenDetach',
      'projectsTokenDetachConfirmTitle',
      'projectsTokenDetachConfirmBody',
      'projectsTokenDetached',
      'projectsTokensColRequests',
      'projectsTokensColPrompt',
      'projectsTokensColGenerated',
      'projectsTokensColTotal',
      'projectsTokensTotalLabel',
      'projectsTokensTotalNote',
      'errorTokenNotFound',
      'systemAdminModeEnter',
      'systemAdminModeLeave',
      'systemAdminModeActive',
      'systemAdminModeDialogTitle',
      'systemAdminModeDialogBody',
      'systemAdminModePasswordLabel',
      'settingsSystemAdminRequirePassword',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });

  it('defines the name-interpolating function in de and en', () => {
    expect(messages.de.projectsMembersTitle('Team Alpha')).toContain('Team Alpha');
    expect(messages.en.projectsMembersTitle('Team Alpha')).toContain('Team Alpha');
  });

  it('defines the tokens-title name-interpolating function in de and en', () => {
    expect(messages.de.projectsTokensTitle('Team Alpha')).toContain('Team Alpha');
    expect(messages.en.projectsTokensTitle('Team Alpha')).toContain('Team Alpha');
  });

  it('defines the coupled-chip name-interpolating function in de and en', () => {
    expect(messages.de.projectsCoupledChip('Team Alpha')).toContain('Team Alpha');
    expect(messages.en.projectsCoupledChip('Team Alpha')).toContain('Team Alpha');
  });
});

describe('copy-feedback i18n key', () => {
  it('defines copied in de and en', () => {
    const keys = ['copied'] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('certificates i18n keys', () => {
  it('defines the nav/module/settings + CA-panel + list + server-override keys in de and en', () => {
    const keys = [
      'confirm',
      'certificates',
      'settingsCertificatesTitle',
      'settingsCertificatesIntro',
      'certificatesSettingsTitle',
      'certificatesMeshTitle',
      'certificatesMeshTLSActive',
      'certificatesMeshTLSInactive',
      'certificatesMeshAddress',
      'certificatesMeshFingerprint',
      'certificatesMeshExpires',
      'certificatesMeshCARotationPendingTitle',
      'certificatesMeshCARotationPending',
      'certificatesMeshRequireTLS',
      'certificatesMeshRequireTLSHint',
      'certificatesMeshRequireTLSNotObserved',
      'certificatesMeshRequireTLSConfirmTitle',
      'certificatesMeshRequireTLSConfirmBody',
      'certificatesMeshRequireTLSPending',
      'certificatesMeshTLSPortMode',
      'certificatesMeshTLSPortModeFollowEnv',
      'certificatesMeshTLSPortModeCombined',
      'certificatesMeshTLSPortModeSeparate',
      'certificatesMeshTLSPort',
      'certificatesMeshTLSPortSeparateActive',
      'certificatesMeshTLSPortSeparateInactive',
      'certificatesMeshTLSPortModeConfirmTitle',
      'certificatesMeshTLSPortModeConfirmBody',
      'settingsCertEnabled',
      'settingsAcmeEmail',
      'settingsAcmeDirectory',
      'settingsAcmeDirectoryProduction',
      'settingsAcmeDirectoryStaging',
      'settingsAcmeDirectoryCustom',
      'settingsAcmeOwnSettings',
      'settingsAcmeWeeklyLimit',
      'settingsAcmeWeeklyLimitUnlimited',
      'settingsCertBaseDomain',
      'settingsCertGatewayDomain',
      'settingsCertServerScope',
      'settingsCertScopeAll',
      'settingsCertScopeSelected',
      'settingsCertManagePublicDomain',
      'settingsCertPublicDomains',
      'settingsCertPublicTitle',
      'settingsCertPublicIssuerMode',
      'settingsCertPublicIssuerModeFollowGlobal',
      'certificatesPublicButtonDownloadBundle',
      'certificatesPublicButtonDownloadKey',
      'settingsCertRenewBeforeDays',
      'settingsCertIssuerMode',
      'settingsCertIssuerAcme',
      'settingsCertIssuerSelfSigned',
      'settingsCertSelfSignedValidity',
      'settingsCertCaRenewBeforeDays',
      'certificatesCaTitle',
      'certificatesCaSubject',
      'certificatesCaNone',
      'certificatesCaDownloadRoot',
      'certificatesCaDownloadBundle',
      'certificatesCaPrevious',
      'certificatesCaRotate',
      'certificatesCaRotateConfirmTitle',
      'certificatesCaRotateConfirmBody',
      'certificatesCaHint',
      'certificatesCaStillNeeded',
      'certificatesReissueAll',
      'certificatesReissueAllConfirmTitle',
      'certificatesReissueAllConfirmBody',
      'certificatesSwitchHint',
      'certificatesColDomain',
      'certificatesColKind',
      'certificatesColStatus',
      'certificatesColIssued',
      'certificatesColExpires',
      'certificatesColRemaining',
      'certificatesColInstalled',
      'certificatesInstalledStale',
      'certificatesInstalledNever',
      'certificatesColTransport',
      'certificatesTransportTLS',
      'certificatesTransportPlain',
      'certificatesTransportNever',
      'certificatesColError',
      'certificatesKindGateway',
      'certificatesKindServer',
      'certificatesKindPublic',
      'certificatesStatusActive',
      'certificatesStatusPending',
      'certificatesStatusError',
      'certificatesStatusSkipped',
      'certificatesRenewNow',
      'certificatesEmpty',
      'certificatesLastErrorTitle',
      'certificatesScopeFlipConfirmTitle',
      'certificatesScopeFlipConfirmBody',
      'certificatesColAttempts',
      'certificatesColNextAttempt',
      'certificatesColFingerprint',
      'certificatesKindEdge',
      'certificatesEdgeIntro',
      'settingsCertEdgeEnabled',
      'settingsCertEdgeIssuerMode',
      'settingsCertEdgeNames',
      'certificatesEdgeNone',
      'certificatesEdgeDeliveryDownloadNote',
      'certificatesEdgeButtonDownloadBundle',
      'certificatesEdgeButtonDownloadKey',
      'certificatesEdgeButtonShowProxyConfig',
      'certificatesEdgeProxyConfigDialogTitle',
      'certificatesEdgeProxyConfigSnapshotNote',
      'certificatesEdgeProxyConfigCopy',
      'certificatesEdgeProxyConfigClose',
      'certificatesEdgeButtonReissue',
      'certificatesEdgeReissueConfirmTitle',
      'certificatesEdgeReissueConfirmBody',
      'certificatesEdgeGateTitle',
      'certificatesEdgeGateIntro',
      'settingsCertEdgeRequireHttps',
      'certificatesEdgeGateDisabledHint',
      'certificatesEdgeGateArmedNotEnforcingHint',
      'certificatesEdgeGateLastEncryptedNever',
      'certificatesEdgeGateLastPlainNever',
      'certificatesEdgeGateProbeButton',
      'certificatesEdgeGateArmConfirmTitle',
      'certificatesEdgeGateArmConfirmBody',
      'errorCertificateEdgeHttpsNotObserved',
      'errorCertificateEdgeArmRequiresHttps',
      'certificatesEdgeProbeBootstrap',
      'certificatesEdgeProbeChainUntrusted',
      'certificatesEdgeProbeExpired',
      'certificatesEdgeProbeNotConfigured',
      'serverCertificateInclude',
      'serverCertificateExclude',
      'certificatesHTTPSSwitchTitle',
      'certificatesHTTPSSwitchMode',
      'certificatesHTTPSSwitchModeManual',
      'certificatesHTTPSSwitchModeAuto',
      'certificatesHTTPSSwitchModeSelected',
      'certificatesHTTPSSwitchModeConfirmTitle',
      'certificatesHTTPSSwitchModeConfirmBody',
      'certificatesProxyListenPortBase',
      'serverHTTPSSwitchInclude',
      'serverHTTPSSwitchExclude',
      'applicationProxyStatus',
      'applicationProxied',
      'applicationNotProxied',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('agent-managed runtime i18n keys (task 19 foundation)', () => {
  it('defines every runtime admin/spec/matrix/limits/status key in de and en', () => {
    const keys = [
      'runtimeAdmin',
      'runtimeSpecs',
      'runtimeSpecEdit',
      'runtimeSpecBinary',
      'runtimeSpecArgs',
      'runtimeSpecEnv',
      'runtimeSpecWorkDir',
      'runtimeSpecGpus',
      'runtimeSpecVram',
      'runtimeSpecIdleTimeout',
      'runtimeSpecStartupTimeout',
      'runtimeSpecPinned',
      'runtimeSpecEnabled',
      'runtimeMatrix',
      'runtimeMatrixHint',
      'runtimeLimits',
      'runtimeGpuBudget',
      'runtimeMaxProcesses',
      'runtimeLiveStatus',
      'runtimeStateStopped',
      'runtimeStateStarting',
      'runtimeStateRunning',
      'runtimeStateDraining',
      'runtimeStateBackoff',
      'runtimeStateStartFailed',
      'runtimeStateCrashed',
      'runtimeStatePendingVram',
      'runtimeStateNotPermitted',
      'runtimeLastError',
      'runtimeForceStart',
      'runtimeForceStop',
      'runtimeClearOverride',
      'runtimeManagedLocally',
      'runtimeManagedOnlyBanner',
      'runtimeIneffectiveSpecs',
      'runtimeTimeoutWarning',
      'runtimeBinaryPathOsMismatchWarning',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });

  it('defines an errorLabelByCode-backed key for every new runtime backend error code in de and en', () => {
    const keys = [
      'errorRuntimeSpecNotFound',
      'errorRuntimeSpecBinaryRequired',
      'errorRuntimeSpecArgsInvalid',
      'errorRuntimeSpecEnvInvalid',
      'errorRuntimeSpecGpuInvalid',
      'errorRuntimeSpecTuningInvalid',
      'errorRuntimeSpecAdminStateInvalid',
      'errorRuntimeSpecApplicationNotServerAgent',
      'errorRuntimeCoresidencyPairInvalid',
      'errorServerGpuBudgetInvalid',
      'errorServerRuntimeLimitInvalid',
      'errorApplicationManagedRuntimeOnly',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('agent-managed runtime i18n keys (task 20 launch specs)', () => {
  it('defines every launch-spec form/list/stub key in de and en', () => {
    const keys = [
      'runtimeSpecsIntro',
      'runtimeSpecCreate',
      'runtimeSpecEditAction',
      'runtimeSpecDelete',
      'runtimeSpecDeleteConfirm',
      'runtimeSpecDeleteStateLoading',
      'runtimeSpecDeleteStateUnknown',
      'runtimeSpecMappingSection',
      'runtimeSpecConfigSection',
      'runtimeSpecListenPort',
      'runtimeSpecListenPortHelp',
      'runtimeSpecHealthPath',
      'runtimeSpecHealthTimeout',
      'runtimeSpecAdmissionWaitTimeout',
      'runtimeSpecAdminState',
      'runtimeSpecVramLocked',
      'runtimeSpecVramLockedHint',
      'runtimeSpecVramMeasured',
      'runtimeSpecGpuIndex',
      'runtimeSpecGpuPick',
      'runtimeSpecGpuPickPlaceholder',
      'runtimeSpecGpuNoTelemetry',
      'runtimeSpecGpuAdd',
      'runtimeSpecGpuRemove',
      'runtimeSpecEnvHint',
      'runtimeSpecEnvReserved',
      'runtimeSpecPlaceholderInvalid',
      'runtimeSpecPartialFailure',
      'runtimeAreaPlaceholder',
      'runtimeStatusUnknown',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('agent-managed runtime i18n keys (task 22 live status + file mode)', () => {
  it('defines every live-status, override, restart-sequence and file-mode key in de and en', () => {
    const keys = [
      'runtimeStreamConnecting',
      'runtimeStreamOpen',
      'runtimeStreamOffline',
      'runtimeStreamError',
      'runtimeStatusEmpty',
      'runtimeStatusSince',
      'runtimeStatusPid',
      'runtimeStatusPort',
      'runtimeStatusInFlight',
      'runtimeStatusRestarts',
      'runtimeLastErrorAt',
      'runtimeLastErrorExitCode',
      'runtimeLastErrorFailures',
      'runtimeLastErrorStderr',
      'runtimeRestart',
      'runtimeRestartStopping',
      'runtimeRestartClearing',
      'runtimeRestartTimeout',
      'runtimeRestartVanished',
      'runtimeParseError',
      // C2 fix round: parse_error is a closed set of CODES, not free
      // text, so the portal owns one sentence per code plus a fallback
      // for a code this build does not know.
      'runtimeParseErrorJsonSyntax',
      'runtimeParseErrorDuplicateSpecId',
      // A1: the two access codes. Missing here, the portal's third seam of
      // the three-sided contract fails silently -- an unmapped code degrades
      // to runtimeParseErrorUnknown, which is safe and says nothing.
      'runtimeParseErrorFileMissing',
      'runtimeParseErrorReadFailed',
      'runtimeParseErrorUnknown',
      'runtimeConfigUnavailable',
      'runtimeConfigUnrecognised',
      'runtimeFeatureMismatch',
      'runtimeAgentVersion',
      'runtimeAgentFeatures',
      'runtimeReportCollectedAt',
      // Fix round 1: the restart-state gate, the bounded writes, the
      // report-failed third state, the gateway/upstream name pair and the
      // "agent never reported" half of the feature-mismatch banner.
      'runtimeRestartClearTimeout',
      'runtimeWriteTimeout',
      'runtimeModeUnknown',
      'runtimeModeUnknownShort',
      'runtimeStatusUpstream',
      'runtimeStatusNameMismatch',
      'runtimeStatusUnresolvedShort',
      'runtimeStatusUnresolved',
      'runtimeAgentNeverReported',
      'resourceRetry',
      // Fix round 2: the hover reason for a disabled Restart. A successful
      // restart leaves the row `stopped`, where Restart is disabled, so this
      // is the resting state of a healthy row and every operator meets it.
      'runtimeRestartUnavailable',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });

  // The nine RuntimeState wire values (server-agent/internal/runtime/
  // types.go) each need a DISTINCT label: the portal has exactly three
  // status colours (theme/ThemeRoot.tsx -- success/watch/standby, no red),
  // so the label is the only thing that can tell "crashed" apart from
  // "stopped" or "not permitted". A duplicated label would silently merge
  // two different facts into one visual.
  it('gives all nine runtime lifecycle states a distinct label in de and en', () => {
    const stateKeys = [
      'runtimeStateStopped',
      'runtimeStateStarting',
      'runtimeStateRunning',
      'runtimeStateDraining',
      'runtimeStateBackoff',
      'runtimeStateStartFailed',
      'runtimeStateCrashed',
      'runtimeStatePendingVram',
      'runtimeStateNotPermitted',
    ] as const;
    for (const locale of ['de', 'en'] as const) {
      const labels = stateKeys.map((k) => messages[locale][k]);
      expect(new Set(labels).size).toBe(stateKeys.length);
    }
  });
});

describe('agent-managed runtime i18n keys (task 21 matrix + limits)', () => {
  it('defines every co-residency-matrix and server-limits key in de and en', () => {
    const keys = [
      'runtimeMatrixCell',
      'runtimeMatrixNeedTwo',
      'runtimeMatrixConsequence',
      'runtimeMatrixNoSharedGpu',
      'runtimeMatrixAdvisory',
      'runtimeMatrixOverBudget',
      'runtimeMatrixDisabledFileMode',
      'runtimeMatrixDisabledSaving',
      'runtimeLimitsIntro',
      'runtimeGpuDriftWarning',
      'runtimeGpuDriftIconLabel',
      'runtimeGpuDriftExpected',
      'runtimeGpuDriftCurrent',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('agent-managed runtime i18n keys (task 22b, batch C)', () => {
  it('defines the failed-resource and duplicate-GPU-index keys in de and en', () => {
    const keys = [
      // C1: the three resources that had no state of their own for a failed
      // GET and said "loading" forever.
      'runtimeMappingsUnavailable',
      'runtimeCoresidencyUnavailable',
      'runtimeBudgetsUnavailable',
      // Fix round 1, M8: the fourth resource, which C1 left on the two-state
      // shape although its title said "every remaining resource".
      'runtimeWarningsUnavailable',
      // Fix round 1, C4/M7: the `stale-error` half. The `*Unavailable` texts
      // above state that nothing is loaded and nothing is possible, which is
      // false once a payload is in hand and only the REFRESH failed -- and all
      // four call sites were rendering them in that state.
      'runtimeMappingsStale',
      'runtimeCoresidencyStale',
      'runtimeBudgetsStale',
      'runtimeWarningsStale',
      // C5: the collision the backend refuses without naming it.
      'runtimeGpuIndexDuplicate',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });
});

describe('runtime-spec arguments-field i18n keys', () => {
  it('defines the args hint, example and the three warning texts in de and en', () => {
    const keys = [
      // The contract the field never stated: one argument per line, a flag and
      // its value on two lines.
      'runtimeSpecArgsHint',
      'runtimeSpecArgsExample',
      // A whole command line pasted onto one line.
      'runtimeSpecArgsCommandLine',
      // A hard-coded port while `listen_port` is 0 and the agent owns it.
      'runtimeSpecArgsHardcodedPort',
      // Whitespace at an argument's edge, and a line that is only whitespace.
      'runtimeSpecArgsEdgeWhitespace',
      'runtimeSpecArgsBlankLine',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });

  // The example is the half that teaches the rule, so its SHAPE is the
  // assertion: separate lines for a flag and its value, ${PORT} rather than a
  // number, and a path whose internal spaces do not split it.
  it('keeps the example one-token-per-line in both locales', () => {
    for (const example of [
      messages.de.runtimeSpecArgsExample,
      messages.en.runtimeSpecArgsExample,
    ]) {
      const lines = example.split('\n');
      expect(lines.length).toBeGreaterThanOrEqual(3);
      expect(lines[0]).toBe('--port');
      expect(lines[1]).toBe('${PORT}');
      expect(lines.some((line) => line.includes(' ') && !line.startsWith('-'))).toBe(true);
    }
  });
});

// The operator-facing strings of the resolved-command block are the ones an
// operator ACTS on, so their scope has to match the scope the code actually
// has. These are content assertions: they pin what the sentences claim, and
// cannot verify that the claim is true -- that is what command.go's own tests
// and §14.7 are for. They exist because this exact string once carried a
// guarantee the dialog rendering it could not keep.
describe('resolved-command masking strings state their own scope', () => {
  it('never claims a masked value does not reach the gateway', () => {
    // False in the very dialog that renders it: a spec with
    // args: ["--api-key", "${AGENT_ENV:HF_TOKEN}"] runs a model server that
    // prints its full argv at startup -- llama.cpp and vLLM both do -- and the
    // agent captures that line into the same stream, ten lines below the alert.
    // last_error.stderr_tail is a second route to the same screen. The scope is
    // "the reported command", exactly as command.go, server-agent/README.md and
    // the architecture doc's §8.3/§14.7 all state it.
    expect(messages.en.runtimeCommandMasked).not.toMatch(/do not reach the gateway/i);
    expect(messages.de.runtimeCommandMasked).not.toMatch(/erreichen das Gateway nicht/i);
  });

  it('scopes the masking to the reported command and names the route that carries a resolved value anyway', () => {
    expect(messages.en.runtimeCommandMasked).toMatch(/this reported command/i);
    expect(messages.en.runtimeCommandMasked).toMatch(/command line or environment at startup/i);
    expect(messages.de.runtimeCommandMasked).toMatch(/hier gemeldeten Befehl/i);
    expect(messages.de.runtimeCommandMasked).toMatch(/Befehlszeile oder Umgebung/i);
  });

  it('keeps the operator guidance that follows from the gap: env, never args', () => {
    for (const m of [messages.en, messages.de]) {
      expect(m.runtimeCommandMasked).toContain('${AGENT_ENV:NAME}');
      expect(m.runtimeCommandMasked).toMatch(/argument/i);
    }
  });

  it('gives the file-mode env redaction a sentence of its own, in both locales', () => {
    // A different reason for withholding -- the agent's specs come from a local
    // document the gateway does not own -- and a different mask. Selecting the
    // placeholder sentence for it told a file-mode operator to go and check a
    // ${AGENT_ENV:NAME} variable that does not exist.
    for (const m of [messages.en, messages.de]) {
      expect(typeof m.runtimeCommandEnvRedacted).toBe('string');
      expect(m.runtimeCommandEnvRedacted.length).toBeGreaterThan(0);
      expect(m.runtimeCommandEnvRedacted).not.toBe(m.runtimeCommandMasked);
    }
  });

  it('words a locally trimmed head differently from one the agent no longer holds', () => {
    // Rendered directly under runtimeLogsTrimmed, which says reopening reloads
    // the agent's retained history in full. The two must not answer each other.
    for (const m of [messages.en, messages.de]) {
      expect(typeof m.runtimeCommandTrimmedHere).toBe('string');
      expect(m.runtimeCommandTrimmedHere).not.toBe(m.runtimeCommandNotRetained);
    }
    expect(messages.en.runtimeCommandTrimmedHere).toMatch(/this view/i);
    expect(messages.de.runtimeCommandTrimmedHere).toMatch(/diese Ansicht/i);
  });
});

describe('one-server_agent-per-server affordance i18n key', () => {
  it('defines applicationTypeServerAgentTaken in de and en', () => {
    for (const m of [messages.de, messages.en]) {
      expect(typeof m.applicationTypeServerAgentTaken).toBe('string');
      expect(m.applicationTypeServerAgentTaken.length).toBeGreaterThan(0);
    }
  });

  it('teaches the rule and the remedy rather than restating the 409', () => {
    // The type field's helper line is read BEFORE the form is filled in, so it
    // has room the toast does not: why the rule exists (one agent per server)
    // and what to do instead (edit or delete the existing application). The
    // 409's own string stays terse because formatPortalError prefixes it with
    // the raw error code. Reusing that string here would have been one key
    // fewer and one sentence that teaches nothing.
    for (const m of [messages.de, messages.en]) {
      expect(m.applicationTypeServerAgentTaken).not.toBe(m.errorApplicationServerAgentExists);
      expect(m.applicationTypeServerAgentTaken.length).toBeGreaterThan(
        m.errorApplicationServerAgentExists.length,
      );
    }
    expect(messages.en.applicationTypeServerAgentTaken).toMatch(/one agent runs per server/i);
    expect(messages.en.applicationTypeServerAgentTaken).toMatch(/edit or delete/i);
    expect(messages.de.applicationTypeServerAgentTaken).toMatch(/genau ein Agent/i);
    expect(messages.de.applicationTypeServerAgentTaken).toMatch(/bearbeiten oder löschen/i);
  });
});

describe('managed_runtime_only affordance i18n keys', () => {
  it('defines applicationTypeManagedRuntimeOnly and runtimeManagedOnlyCreateBlocked in de and en', () => {
    const keys = ['applicationTypeManagedRuntimeOnly', 'runtimeManagedOnlyCreateBlocked'] as const;
    for (const m of [messages.de, messages.en]) {
      for (const k of keys) {
        expect(typeof m[k]).toBe('string');
        expect(m[k].length).toBeGreaterThan(0);
      }
    }
  });

  it('says on the type field that the gate is create-only, and does not restate the 409', () => {
    // The backend reads ManagedRuntimeOnly inside CreateApplication only;
    // UpdateApplication never reads it. The helper line is the one place an
    // operator can be told that, so it must say it -- otherwise the edit form
    // offering all six types reads as a portal bug rather than as the rule.
    // The 409's own string stays terse (formatPortalError prefixes it with the
    // raw error code); reusing it here would teach nothing.
    for (const m of [messages.de, messages.en]) {
      expect(m.applicationTypeManagedRuntimeOnly).not.toBe(m.errorApplicationManagedRuntimeOnly);
      expect(m.applicationTypeManagedRuntimeOnly.length).toBeGreaterThan(
        m.errorApplicationManagedRuntimeOnly.length,
      );
    }
    expect(messages.en.applicationTypeManagedRuntimeOnly).toMatch(/server_agent/);
    expect(messages.en.applicationTypeManagedRuntimeOnly).toMatch(/creating only/i);
    expect(messages.en.applicationTypeManagedRuntimeOnly).toMatch(/stay editable/i);
    expect(messages.de.applicationTypeManagedRuntimeOnly).toMatch(/server_agent/);
    expect(messages.de.applicationTypeManagedRuntimeOnly).toMatch(/fürs Anlegen/i);
    expect(messages.de.applicationTypeManagedRuntimeOnly).toMatch(/änderbar/i);
  });

  it('gives the vanished create button its own sentence, separate from the standing banner', () => {
    // The banner states the server's mode and is always on; this second
    // sentence states a restriction that is only in force once the one
    // server_agent application exists, so it must be a different string --
    // folding them into one would assert the restriction unconditionally.
    for (const m of [messages.de, messages.en]) {
      expect(m.runtimeManagedOnlyCreateBlocked).not.toBe(m.runtimeManagedOnlyBanner);
      expect(m.runtimeManagedOnlyCreateBlocked.length).toBeGreaterThan(
        m.runtimeManagedOnlyBanner.length,
      );
    }
    expect(messages.en.runtimeManagedOnlyCreateBlocked).toMatch(/already exists/i);
    expect(messages.en.runtimeManagedOnlyCreateBlocked).toMatch(/edit or delete/i);
    expect(messages.de.runtimeManagedOnlyCreateBlocked).toMatch(/bereits angelegt/i);
    expect(messages.de.runtimeManagedOnlyCreateBlocked).toMatch(/bearbeiten oder löschen/i);
  });
});

describe('managed_runtime_only server-form control i18n keys', () => {
  it('defines serverManagedRuntimeOnlyLabel and serverManagedRuntimeOnlyHelp in de and en', () => {
    const keys = ['serverManagedRuntimeOnlyLabel', 'serverManagedRuntimeOnlyHelp'] as const;
    for (const m of [messages.de, messages.en]) {
      for (const k of keys) {
        expect(typeof m[k]).toBe('string');
        expect(m[k].length).toBeGreaterThan(0);
      }
    }
  });

  it('states the two consequences an operator cannot guess: not retroactive, and the create button vanishes', () => {
    // This checkbox turns a server into one that refuses most application
    // creates. Neither consequence is visible from the label, and both are
    // reachable only from this one string -- an operator who reads only the
    // label would expect the flag to disable the applications that already
    // exist (it does not: the backend reads it inside CreateApplication only),
    // and would not expect the create button to disappear once the server's
    // one server_agent application exists (managed_runtime_only permits only
    // that type and the one-agent-per-server rule refuses a second of it, so
    // the two gates intersect to the empty set).
    expect(messages.en.serverManagedRuntimeOnlyHelp).toMatch(/server_agent/);
    expect(messages.en.serverManagedRuntimeOnlyHelp).toMatch(/not retroactive/i);
    expect(messages.en.serverManagedRuntimeOnlyHelp).toMatch(/stay editable/i);
    expect(messages.en.serverManagedRuntimeOnlyHelp).toMatch(/create button/i);
    expect(messages.de.serverManagedRuntimeOnlyHelp).toMatch(/server_agent/);
    expect(messages.de.serverManagedRuntimeOnlyHelp).toMatch(/nicht rückwirkend/i);
    expect(messages.de.serverManagedRuntimeOnlyHelp).toMatch(/änderbar/i);
    expect(messages.de.serverManagedRuntimeOnlyHelp).toMatch(/Schaltfläche zum Anlegen/i);
  });

  it('does not contradict the application-side affordance strings', () => {
    // Three strings now describe the same rule from two screens. The server
    // form's help is the only one that must carry BOTH consequences (it is the
    // control that causes them), so it is the longest; the other two are read
    // in a context that already supplies half the answer. If a future edit
    // shortens this one below either, a consequence was dropped.
    for (const m of [messages.de, messages.en]) {
      expect(m.serverManagedRuntimeOnlyHelp.length).toBeGreaterThan(
        m.applicationTypeManagedRuntimeOnly.length,
      );
      expect(m.serverManagedRuntimeOnlyHelp.length).toBeGreaterThan(
        m.runtimeManagedOnlyCreateBlocked.length,
      );
      // The label names the restriction; it must not itself be the explanation.
      expect(m.serverManagedRuntimeOnlyLabel.length).toBeLessThan(
        m.serverManagedRuntimeOnlyHelp.length,
      );
    }
  });
});

describe('per-application TLS-proxy opt-out i18n keys', () => {
  const stringKeys = [
    'applicationProxyChipExcluded',
    'applicationProxyLegend',
    'applicationProxyExcluded',
    'applicationProxyExcludedNote',
    'applicationProxyModeActiveNote',
    'applicationProxyModeOffNote',
    'applicationProxyModeUnknownNote',
    'applicationProxyOutOfScopeNote',
    'applicationSchemeManagedNote',
    'applicationProxyPortExcluded',
    'applicationProxyPortUnassigned',
  ] as const;

  it('defines every new key in de and en', () => {
    for (const k of stringKeys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });

  it('gives the three proxy-state sentences three DISTINCT texts, in both locales', () => {
    // Reusing one sentence across the states would tell the operator "the
    // control is here" without telling them why, which is the whole point of
    // rendering the third state at all.
    for (const locale of ['de', 'en'] as const) {
      const notes = new Set([
        messages[locale].applicationProxyModeActiveNote,
        messages[locale].applicationProxyModeOffNote,
        messages[locale].applicationProxyModeUnknownNote,
        messages[locale].applicationProxyOutOfScopeNote,
      ]);
      expect(notes.size).toBe(4);
    }
  });

  it('interpolates the assigned port and the server domain in both locales', () => {
    for (const locale of ['de', 'en'] as const) {
      expect(messages[locale].applicationProxyPort(8601)).toContain('8601');
      expect(messages[locale].applicationProxyOwnTLSWarning('srv.example.test')).toContain(
        'srv.example.test',
      );
    }
  });

  it('never renders an unassigned port as a bare 0, a blank or a dash', () => {
    for (const locale of ['de', 'en'] as const) {
      const text = messages[locale].applicationProxyPortUnassigned;
      expect(text.length).toBeGreaterThan(20);
      expect(text).not.toMatch(/:\s*(0|-|—)\s*$/);
    }
  });
});
