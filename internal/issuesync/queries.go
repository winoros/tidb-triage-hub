package issuesync

const IssueFieldsFragment = `
fragment IssueFields on Issue {
  number
  title
  state
  stateReason
  createdAt
  updatedAt
  closedAt
  author { login }
  assignees(first: 10) { nodes { login } }
  labels(first: 30) { nodes { name } }
  timelineItems(itemTypes: [CROSS_REFERENCED_EVENT, CONNECTED_EVENT], last: 100) {
    totalCount
    nodes {
      __typename
      ... on CrossReferencedEvent {
        source {
          __typename
          ... on PullRequest {
            number
            title
            state
            isDraft
            baseRefName
            url
            updatedAt
            mergedAt
            labels(first: 10) { nodes { name } }
            repository { nameWithOwner }
          }
        }
      }
      ... on ConnectedEvent {
        subject {
          __typename
          ... on PullRequest {
            number
            title
            state
            isDraft
            baseRefName
            url
            updatedAt
            mergedAt
            labels(first: 10) { nodes { name } }
            repository { nameWithOwner }
          }
        }
      }
    }
  }
}
`

const IssueDetailFieldsFragment = IssueFieldsFragment + `
fragment IssueDetailFields on Issue {
  ...IssueFields
  body
  comments(last: 15) {
    totalCount
    nodes {
      author { login }
      createdAt
      body
    }
  }
}
`

const SearchIssuesGQL = `
query($q: String!, $cursor: String) {
  rateLimit { limit cost remaining used resetAt }
  search(query: $q, type: ISSUE, first: 100, after: $cursor) {
    issueCount
    pageInfo {
      hasNextPage
      endCursor
    }
    nodes {
      ... on Issue {
        number
        title
        state
        stateReason
        createdAt
        updatedAt
        closedAt
        author { login }
        labels(first: 30) { nodes { name } }
      }
    }
  }
}
`

const SearchPRsGQL = `
query($q: String!, $cursor: String) {
  rateLimit { limit cost remaining used resetAt }
  search(query: $q, type: ISSUE, first: 50, after: $cursor) {
    issueCount
    pageInfo {
      hasNextPage
      endCursor
    }
    nodes {
      ... on PullRequest {
        number
        title
        body
        url
        baseRefName
        state
        isDraft
        updatedAt
        mergedAt
        labels(first: 10) { nodes { name } }
        repository { nameWithOwner }
        closingIssuesReferences(first: 20) {
          totalCount
          nodes {
            number
            repository { nameWithOwner }
          }
        }
        commits(last: 15) {
          totalCount
          nodes {
            commit {
              messageHeadline
              messageBody
            }
          }
        }
        comments(last: 10) {
          totalCount
          nodes {
            body
          }
        }
      }
    }
  }
}
`
