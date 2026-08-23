// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { Translation } from './shared/types';
import { PageTitle } from './shared/PageTitle';

export function SectionStub({ title, t }: Readonly<{ title: string; t: Translation }>) {
  return <PageTitle title={title} subtitle={t.stubIntro} />;
}
