// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

import apollo from '@/utils/apolloManager';
import gql from 'graphql-tag';
import { ApiKey } from '@/types/apiKeys';
import { RequestResponse } from '@/types/response';

export async function fetchAPIKeys(projectId: string): Promise<RequestResponse<ApiKey[]>> {
    let result: RequestResponse<ApiKey[]> = new RequestResponse<ApiKey[]>();

    let response: any = await apollo.query({
        query: gql(`
            query($projectId: String!) {
                project(
                    id: $projectId,
                ) {
                    apiKeys {
                        id,
                        name,
                        createdAt
                    }
                }
            }`
        ),
        variables: {
            projectId: projectId
        },
        fetchPolicy: 'no-cache',
        errorPolicy: 'all',
    });

    if (response.errors) {
        result.errorMessage = response.errors[0].message;
    } else {
        result.isSuccess = true;
        result.data = getApiKeysList(response.data.project.apiKeys);
    }

    return result;
}

export async function createAPIKey(projectId: string, name: string): Promise<RequestResponse<ApiKey>> {
    let result: RequestResponse<ApiKey> = new RequestResponse<ApiKey>();

    let response: any = await apollo.mutate({
        mutation: gql(`
            mutation($projectId: String!, $name: String!) {
                createAPIKey(
                    projectID: $projectId,
                    name: $name
                ) {
                    key,
                    keyInfo {
                        id,
                        name,
                        createdAt
                    }
                }
            }`
        ),
        variables: {
            projectId: projectId,
            name: name
        },
        fetchPolicy: 'no-cache',
        errorPolicy: 'all',
    });

    if (response.errors) {
        result.errorMessage = response.errors[0].message;
    } else {
        result.isSuccess = true;
        let key: any = response.data.createAPIKey.keyInfo;
        let secret: string = response.data.createAPIKey.key;

        result.data = new ApiKey(key.id, key.name, key.createdAt, secret);
    }

    return result;
}

export async function deleteAPIKeys(ids: string[]): Promise<RequestResponse<null>> {
    // TODO: find needed type instead of any
    let result: RequestResponse<any> = new RequestResponse<any>();

    let response: any = await apollo.mutate({
        mutation: gql(
            `mutation($id: [String!]!) {
                deleteAPIKeys(id: $id) {
                    id
                }
            }`
        ),
        variables:{
            id: ids
        },
        fetchPolicy: 'no-cache',
        errorPolicy: 'all',
    });

    if (response.errors) {
        result.errorMessage = response.errors[0].message;
    } else {
        result.isSuccess = true;
        result.data = response.data.deleteAPIKeys;
    }

    return result;
}

function getApiKeysList(apiKeys: ApiKey[]): ApiKey[] {
    if (!apiKeys) {
        return [];
    }

    return apiKeys.map(key => new ApiKey(key.id, key.name, key.createdAt, ''));
}
