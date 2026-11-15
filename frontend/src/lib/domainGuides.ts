/**
 * 平台域名验证静态知识数据（域名治理页，纯前端展示，不做实时 DNS 检测）。
 *
 * 取舍与口径：
 * - 各平台的「记录值」分两类：`fixed: true` 为平台固定模板值（可直接复制），
 *   `fixed: false` 为按域名/账号在平台控制台生成的值（DKIM 公钥、子域专属值等），
 *   页面只展示示例形态，复制得到的是示例行，**实际值一律以平台控制台为准**。
 * - host 为相对主域名的相对记录名（如 `send`、`_dmarc`、`k1._domainkey`）；
 *   平台常用 mail-from 子域（如 `mg.example.com`）承载 SPF/MX，desc 中注明。
 * - 数据来源：官方文档核对（Resend/Mailjet 已核对记录类型与形态；其余平台按
 *   官方控制台展示形态整理），存在平台随版本调整的可能，页面恒标注
 *   「以平台控制台为准」，本数据仅作配置指引而非权威值。
 * - i18n：用户可见文案一律存 domains 词典 key（下方 as const 数据推导出字面量
 *   联合，tsc 强校验 key 存在性）；消费处（DomainsPage）经 t(key) 渲染/拼接。
 *   品牌名、协议名与 DNS 记录值本身不翻译——词典 zh/en 对这些 key 取值一致。
 */

/** 验证记录类别（域名治理矩阵的「记录类型」维度；显示名见词典 kind.* 组） */
export type DomainRecordKind = 'SPF' | 'DKIM' | 'DMARC' | 'MX' | 'senderVerify'

/** 记录类别 → 词典 key（domains ns 的 kind.* 组，zh/en 值即类别显示名） */
export const KIND_KEYS = {
  SPF: 'kind.SPF',
  DKIM: 'kind.DKIM',
  DMARC: 'kind.DMARC',
  MX: 'kind.MX',
  senderVerify: 'kind.senderVerify',
} as const

/** 通用兜底指引（未知 provider / 通用 SMTP / Mock 复用）：文案字段为词典 key */
const GENERIC_GUIDE = {
  provider: 'generic',
  label: 'generic.label',
  summary: 'generic.summary',
  records: [
    {
      kind: 'SPF',
      host: 'shared.rootHost',
      value: 'generic.spfValue',
      fixed: false,
      required: false,
      desc: 'generic.spfDesc',
    },
    {
      kind: 'DKIM',
      host: 'shared.selectorHost',
      value: 'generic.dkimValue',
      fixed: false,
      required: false,
      desc: 'generic.dkimDesc',
    },
    {
      kind: 'DMARC',
      host: 'shared.dmarcHost',
      value: 'shared.dmarcValue',
      fixed: true,
      required: false,
      desc: 'generic.dmarcDesc',
    },
    {
      kind: 'senderVerify',
      host: 'generic.senderHost',
      value: 'generic.senderValue',
      fixed: true,
      required: true,
      desc: 'generic.senderDesc',
    },
  ],
  consoleHint: 'generic.consoleHint',
} as const

/** 各平台指引数据（文案字段为词典 key；同一字符串多处出现共用 shared.* key） */
const DOMAIN_GUIDE_DATA = {
  resend: {
    provider: 'resend',
    label: 'resend.label',
    summary: 'resend.summary',
    records: [
      {
        kind: 'SPF',
        host: 'resend.returnPathHost',
        value: 'resend.spfValue',
        fixed: true,
        required: true,
        desc: 'resend.spfDesc',
      },
      {
        kind: 'MX',
        host: 'resend.returnPathHost',
        value: 'resend.mxValue',
        fixed: false,
        required: true,
        desc: 'resend.mxDesc',
      },
      {
        kind: 'DKIM',
        host: 'resend.dkimHost',
        value: 'resend.dkimValue',
        fixed: false,
        required: true,
        desc: 'resend.dkimDesc',
      },
      {
        kind: 'DMARC',
        host: 'shared.dmarcHost',
        value: 'shared.dmarcValue',
        fixed: true,
        required: false,
        desc: 'resend.dmarcDesc',
      },
    ],
    consoleHint: 'resend.consoleHint',
  },
  mailgun: {
    provider: 'mailgun',
    label: 'mailgun.label',
    summary: 'mailgun.summary',
    records: [
      {
        kind: 'SPF',
        host: 'mailgun.mailFromHost',
        value: 'mailgun.spfValue',
        fixed: true,
        required: true,
        desc: 'mailgun.spfDesc',
      },
      {
        kind: 'MX',
        host: 'mailgun.mailFromHost',
        value: 'mailgun.mxValue',
        fixed: true,
        required: true,
        desc: 'mailgun.mxDesc',
      },
      {
        kind: 'DKIM',
        host: 'shared.k1Host',
        value: 'shared.dkimPublicKey',
        fixed: false,
        required: true,
        desc: 'mailgun.dkimDesc',
      },
      {
        kind: 'DMARC',
        host: 'shared.dmarcHost',
        value: 'shared.dmarcValue',
        fixed: true,
        required: false,
        desc: 'shared.dmarcDescShort',
      },
    ],
    consoleHint: 'mailgun.consoleHint',
  },
  brevo: {
    provider: 'brevo',
    label: 'brevo.label',
    summary: 'brevo.summary',
    records: [
      {
        kind: 'DKIM',
        host: 'brevo.dkimHost',
        value: 'brevo.dkimValue',
        fixed: false,
        required: true,
        desc: 'brevo.dkimDesc',
      },
      {
        kind: 'senderVerify',
        host: 'brevo.senderHost',
        value: 'brevo.senderValue',
        fixed: true,
        required: true,
        desc: 'brevo.senderDesc',
      },
      {
        kind: 'DMARC',
        host: 'shared.dmarcHost',
        value: 'shared.dmarcValue',
        fixed: true,
        required: false,
        desc: 'brevo.dmarcDesc',
      },
    ],
    consoleHint: 'brevo.consoleHint',
  },
  smtp2go: {
    provider: 'smtp2go',
    label: 'smtp2go.label',
    summary: 'smtp2go.summary',
    records: [
      {
        kind: 'SPF',
        host: 'shared.rootHost',
        value: 'smtp2go.spfValue',
        fixed: false,
        required: true,
        desc: 'smtp2go.spfDesc',
      },
      {
        kind: 'DKIM',
        host: 'shared.selectorHost',
        value: 'smtp2go.dkimValue',
        fixed: false,
        required: true,
        desc: 'smtp2go.dkimDesc',
      },
      {
        kind: 'senderVerify',
        host: 'smtp2go.senderHost',
        value: 'shared.verifiedValue',
        fixed: true,
        required: true,
        desc: 'smtp2go.senderDesc',
      },
      {
        kind: 'DMARC',
        host: 'shared.dmarcHost',
        value: 'shared.dmarcValue',
        fixed: true,
        required: false,
        desc: 'shared.dmarcDescShort',
      },
    ],
    consoleHint: 'smtp2go.consoleHint',
  },
  mailjet: {
    provider: 'mailjet',
    label: 'mailjet.label',
    summary: 'mailjet.summary',
    records: [
      {
        kind: 'SPF',
        host: 'shared.rootHost',
        value: 'mailjet.spfValue',
        fixed: false,
        required: true,
        desc: 'mailjet.spfDesc',
      },
      {
        kind: 'DKIM',
        host: 'shared.k1Host',
        value: 'shared.dkimPublicKey',
        fixed: false,
        required: true,
        desc: 'mailjet.dkimDesc',
      },
      {
        kind: 'senderVerify',
        host: 'mailjet.senderHost',
        value: 'mailjet.senderValue',
        fixed: true,
        required: true,
        desc: 'mailjet.senderDesc',
      },
      {
        kind: 'DMARC',
        host: 'shared.dmarcHost',
        value: 'shared.dmarcValue',
        fixed: true,
        required: false,
        desc: 'shared.dmarcDescShort',
      },
    ],
    consoleHint: 'mailjet.consoleHint',
  },
  elasticemail: {
    provider: 'elasticemail',
    label: 'elasticemail.label',
    summary: 'elasticemail.summary',
    records: [
      {
        kind: 'SPF',
        host: 'shared.rootHost',
        value: 'shared.spfConsoleInclude',
        fixed: false,
        required: true,
        desc: 'shared.spfMergeDesc',
      },
      {
        kind: 'DKIM',
        host: 'shared.selectorHost',
        value: 'shared.consoleGeneratedValue',
        fixed: false,
        required: true,
        desc: 'elasticemail.dkimDesc',
      },
      {
        kind: 'senderVerify',
        host: 'elasticemail.senderHost',
        value: 'elasticemail.senderValue',
        fixed: true,
        required: true,
        desc: 'elasticemail.senderDesc',
      },
      {
        kind: 'DMARC',
        host: 'shared.dmarcHost',
        value: 'shared.dmarcValue',
        fixed: true,
        required: false,
        desc: 'shared.dmarcDescShort',
      },
    ],
    consoleHint: 'elasticemail.consoleHint',
  },
  mailtrap: {
    provider: 'mailtrap',
    label: 'mailtrap.label',
    summary: 'mailtrap.summary',
    records: [
      {
        kind: 'SPF',
        host: 'shared.rootHost',
        value: 'shared.spfConsoleInclude',
        fixed: false,
        required: true,
        desc: 'shared.spfMergeDesc',
      },
      {
        kind: 'DKIM',
        host: 'shared.selectorHost',
        value: 'shared.consoleGeneratedValue',
        fixed: false,
        required: true,
        desc: 'mailtrap.dkimDesc',
      },
      {
        kind: 'DMARC',
        host: 'shared.dmarcHost',
        value: 'shared.dmarcValue',
        fixed: true,
        required: false,
        desc: 'mailtrap.dmarcDesc',
      },
      {
        kind: 'senderVerify',
        host: 'mailtrap.senderHost',
        value: 'shared.verifiedValue',
        fixed: true,
        required: true,
        desc: 'mailtrap.senderDesc',
      },
    ],
    consoleHint: 'mailtrap.consoleHint',
  },
  aliyun: {
    provider: 'aliyun',
    label: 'aliyun.label',
    summary: 'aliyun.summary',
    records: [
      {
        kind: 'SPF',
        host: 'shared.rootHost',
        value: 'aliyun.spfValue',
        fixed: false,
        required: true,
        desc: 'aliyun.spfDesc',
      },
      {
        kind: 'MX',
        host: 'shared.rootHost',
        value: 'aliyun.mxValue',
        fixed: false,
        required: true,
        desc: 'aliyun.mxDesc',
      },
      {
        kind: 'DKIM',
        host: 'shared.selectorHost',
        value: 'shared.consoleGeneratedValue',
        fixed: false,
        required: true,
        desc: 'aliyun.dkimDesc',
      },
      {
        kind: 'senderVerify',
        host: 'aliyun.senderHost',
        value: 'aliyun.senderValue',
        fixed: true,
        required: true,
        desc: 'aliyun.senderDesc',
      },
    ],
    consoleHint: 'aliyun.consoleHint',
  },
  smtp: {
    provider: 'smtp',
    label: 'smtp.label',
    summary: 'smtp.summary',
    records: GENERIC_GUIDE.records,
    consoleHint: 'generic.consoleHint',
  },
} as const

/** 指引数据全集（含兜底 generic）：推导词典 key 字面量联合的类型源 */
type GuideData = typeof GENERIC_GUIDE | (typeof DOMAIN_GUIDE_DATA)[keyof typeof DOMAIN_GUIDE_DATA]

/**
 * domains 词典 key 字面量联合：指引数据里全部文案 key（label/summary/
 * consoleHint/记录 host·value·desc/kind.*），消费处 t(key) 据此强类型校验。
 */
export type DomainGuideKey =
  | (typeof KIND_KEYS)[DomainRecordKind]
  | GuideData['label']
  | GuideData['summary']
  | GuideData['consoleHint']
  | GuideData['records'][number]['host' | 'value' | 'desc']

/** 单条验证记录的指引（文案字段为 domains 词典 key，消费处 t(key) 解析） */
export interface DomainGuideRecord {
  kind: DomainRecordKind
  /** 相对主域名的记录名（词典 key，如 "send"、"_dmarc"、"k1._domainkey"） */
  host: DomainGuideKey
  /** 记录值（固定模板值或示例形态；词典 key） */
  value: DomainGuideKey
  /** true = 平台固定值可直接使用；false = 控制台按域名生成，以控制台为准 */
  fixed: boolean
  /** 是否平台强制要求（false = 推荐/可选） */
  required: boolean
  /** 说明文案（词典 key） */
  desc: DomainGuideKey
}

/** 单个平台的域名验证要求与指引（文案字段为 domains 词典 key） */
export interface PlatformDomainGuide {
  /** 对应渠道 provider 标识（model.Channel.Provider） */
  provider: string
  /** 平台显示名（词典 key） */
  label: DomainGuideKey
  /** 一句话概述：该平台对发件域名的要求（词典 key） */
  summary: DomainGuideKey
  /** 需要配置的验证记录 */
  records: readonly DomainGuideRecord[]
  /** 控制台入口提示（含「以平台控制台为准」口径；词典 key） */
  consoleHint: DomainGuideKey
}

export const DOMAIN_GUIDES: Record<string, PlatformDomainGuide> = DOMAIN_GUIDE_DATA

/** 取平台指引：未知 provider（含 mock）回落通用指引 */
export function getDomainGuide(provider: string): PlatformDomainGuide {
  return DOMAIN_GUIDES[provider] ?? GENERIC_GUIDE
}

/** 解析渠道 allowed_domains（逗号分隔）为去重小写域名列表；空 = 渠道不限制 */
export function parseAllowedDomains(raw: string | undefined): string[] {
  if (!raw) return []
  return [...new Set(
    raw
      .split(',')
      .map((d) => d.trim().toLowerCase())
      .filter((d) => d.length > 0),
  )]
}
