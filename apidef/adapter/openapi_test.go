package adapter

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/buger/jsonparser"
	"github.com/stretchr/testify/require"

	"github.com/TykTechnologies/tyk/apidef"
)

const petstoreExpandedOpenAPI3 = `openapi: "3.0.0"
info:
  version: 1.0.0
  title: Swagger Petstore
  description: A sample API that uses a petstore as an example to demonstrate features in the OpenAPI 3.0 specification
  termsOfService: http://swagger.io/terms/
  contact:
    name: Swagger API Team
    email: apiteam@swagger.io
    url: http://swagger.io
  license:
    name: Apache 2.0
    url: https://www.apache.org/licenses/LICENSE-2.0.html
servers:
  - url: http://petstore.swagger.io/api
paths:
  /pets:
    get:
      description: |
        Returns all pets from the system that the user has access to
        Nam sed condimentum est. Maecenas tempor sagittis sapien, nec rhoncus sem sagittis sit amet. Aenean at gravida augue, ac iaculis sem. Curabitur odio lorem, ornare eget elementum nec, cursus id lectus. Duis mi turpis, pulvinar ac eros ac, tincidunt varius justo. In hac habitasse platea dictumst. Integer at adipiscing ante, a sagittis ligula. Aenean pharetra tempor ante molestie imperdiet. Vivamus id aliquam diam. Cras quis velit non tortor eleifend sagittis. Praesent at enim pharetra urna volutpat venenatis eget eget mauris. In eleifend fermentum facilisis. Praesent enim enim, gravida ac sodales sed, placerat id erat. Suspendisse lacus dolor, consectetur non augue vel, vehicula interdum libero. Morbi euismod sagittis libero sed lacinia.

        Sed tempus felis lobortis leo pulvinar rutrum. Nam mattis velit nisl, eu condimentum ligula luctus nec. Phasellus semper velit eget aliquet faucibus. In a mattis elit. Phasellus vel urna viverra, condimentum lorem id, rhoncus nibh. Ut pellentesque posuere elementum. Sed a varius odio. Morbi rhoncus ligula libero, vel eleifend nunc tristique vitae. Fusce et sem dui. Aenean nec scelerisque tortor. Fusce malesuada accumsan magna vel tempus. Quisque mollis felis eu dolor tristique, sit amet auctor felis gravida. Sed libero lorem, molestie sed nisl in, accumsan tempor nisi. Fusce sollicitudin massa ut lacinia mattis. Sed vel eleifend lorem. Pellentesque vitae felis pretium, pulvinar elit eu, euismod sapien.
      operationId: findPets
      parameters:
        - name: tags
          in: query
          description: tags to filter by
          required: false
          style: form
          schema:
            type: array
            items:
              type: string
        - name: limit
          in: query
          description: maximum number of results to return
          required: false
          schema:
            type: integer
            format: int32
      responses:
        '200':
          description: pet response
          content:
            application/json:
              schema:
                type: array
                items:
                  $ref: '#/components/schemas/Pet'
        default:
          description: unexpected error
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'
    post:
      description: Creates a new pet in the store. Duplicates are allowed
      operationId: addPet
      requestBody:
        description: Pet to add to the store
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/NewPet'
      responses:
        '200':
          description: pet response
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Pet'
        default:
          description: unexpected error
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'
  /pets/{id}:
    get:
      description: Returns a user based on a single ID, if the user does not have access to the pet
      operationId: find pet by id
      parameters:
        - name: id
          in: path
          description: ID of pet to fetch
          required: true
          schema:
            type: integer
            format: int64
      responses:
        '200':
          description: pet response
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Pet'
        default:
          description: unexpected error
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'
    delete:
      description: deletes a single pet based on the ID supplied
      operationId: deletePet
      parameters:
        - name: id
          in: path
          description: ID of pet to delete
          required: true
          schema:
            type: integer
            format: int64
      responses:
        '204':
          description: pet deleted
        default:
          description: unexpected error
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'
components:
  schemas:
    Pet:
      allOf:
        - $ref: '#/components/schemas/NewPet'
        - type: object
          required:
            - id
          properties:
            id:
              type: integer
              format: int64

    NewPet:
      type: object
      required:
        - name
      properties:
        name:
          type: string
        tag:
          type: string

    Error:
      type: object
      required:
        - code
        - message
      properties:
        code:
          type: integer
          format: int32
        message:
          type: string`

const expectedOpenAPIGraphQLSchema = "schema {\n    query: Query\n    mutation: Mutation\n}\n\ntype Query {\n    \"Returns a user based on a single ID, if the user does not have access to the pet\"\n    findPetById(id: Int!): Pet\n    \"\"\"\n    Returns all pets from the system that the user has access to\n    Nam sed condimentum est. Maecenas tempor sagittis sapien, nec rhoncus sem sagittis sit amet. Aenean at gravida augue, ac iaculis sem. Curabitur odio lorem, ornare eget elementum nec, cursus id lectus. Duis mi turpis, pulvinar ac eros ac, tincidunt varius justo. In hac habitasse platea dictumst. Integer at adipiscing ante, a sagittis ligula. Aenean pharetra tempor ante molestie imperdiet. Vivamus id aliquam diam. Cras quis velit non tortor eleifend sagittis. Praesent at enim pharetra urna volutpat venenatis eget eget mauris. In eleifend fermentum facilisis. Praesent enim enim, gravida ac sodales sed, placerat id erat. Suspendisse lacus dolor, consectetur non augue vel, vehicula interdum libero. Morbi euismod sagittis libero sed lacinia.\n\n    Sed tempus felis lobortis leo pulvinar rutrum. Nam mattis velit nisl, eu condimentum ligula luctus nec. Phasellus semper velit eget aliquet faucibus. In a mattis elit. Phasellus vel urna viverra, condimentum lorem id, rhoncus nibh. Ut pellentesque posuere elementum. Sed a varius odio. Morbi rhoncus ligula libero, vel eleifend nunc tristique vitae. Fusce et sem dui. Aenean nec scelerisque tortor. Fusce malesuada accumsan magna vel tempus. Quisque mollis felis eu dolor tristique, sit amet auctor felis gravida. Sed libero lorem, molestie sed nisl in, accumsan tempor nisi. Fusce sollicitudin massa ut lacinia mattis. Sed vel eleifend lorem. Pellentesque vitae felis pretium, pulvinar elit eu, euismod sapien.\n    \"\"\"\n    findPets(limit: Int, tags: [String]): [Pet]\n}\n\ntype Mutation {\n    \"Creates a new pet in the store. Duplicates are allowed\"\n    addPet(newPetInput: NewPetInput!): Pet\n    \"deletes a single pet based on the ID supplied\"\n    deletePet(id: Int!): String\n}\n\ninput NewPetInput {\n    name: String!\n    tag: String\n}\n\ntype Pet {\n    id: Int!\n    name: String!\n    tag: String\n}"

const expectedOpenAPIGraphQLConfig = `{
    "enabled": true,
    "execution_mode": "executionEngine",
    "version": "2",
    "schema": "schema {\n    query: Query\n    mutation: Mutation\n}\n\ntype Query {\n    \"Returns a user based on a single ID, if the user does not have access to the pet\"\n    findPetById(id: Int!): Pet\n    \"\"\"\n    Returns all pets from the system that the user has access to\n    Nam sed condimentum est. Maecenas tempor sagittis sapien, nec rhoncus sem sagittis sit amet. Aenean at gravida augue, ac iaculis sem. Curabitur odio lorem, ornare eget elementum nec, cursus id lectus. Duis mi turpis, pulvinar ac eros ac, tincidunt varius justo. In hac habitasse platea dictumst. Integer at adipiscing ante, a sagittis ligula. Aenean pharetra tempor ante molestie imperdiet. Vivamus id aliquam diam. Cras quis velit non tortor eleifend sagittis. Praesent at enim pharetra urna volutpat venenatis eget eget mauris. In eleifend fermentum facilisis. Praesent enim enim, gravida ac sodales sed, placerat id erat. Suspendisse lacus dolor, consectetur non augue vel, vehicula interdum libero. Morbi euismod sagittis libero sed lacinia.\n\n    Sed tempus felis lobortis leo pulvinar rutrum. Nam mattis velit nisl, eu condimentum ligula luctus nec. Phasellus semper velit eget aliquet faucibus. In a mattis elit. Phasellus vel urna viverra, condimentum lorem id, rhoncus nibh. Ut pellentesque posuere elementum. Sed a varius odio. Morbi rhoncus ligula libero, vel eleifend nunc tristique vitae. Fusce et sem dui. Aenean nec scelerisque tortor. Fusce malesuada accumsan magna vel tempus. Quisque mollis felis eu dolor tristique, sit amet auctor felis gravida. Sed libero lorem, molestie sed nisl in, accumsan tempor nisi. Fusce sollicitudin massa ut lacinia mattis. Sed vel eleifend lorem. Pellentesque vitae felis pretium, pulvinar elit eu, euismod sapien.\n    \"\"\"\n    findPets(limit: Int, tags: [String]): [Pet]\n}\n\ntype Mutation {\n    \"Creates a new pet in the store. Duplicates are allowed\"\n    addPet(newPetInput: NewPetInput!): Pet\n    \"deletes a single pet based on the ID supplied\"\n    deletePet(id: Int!): String\n}\n\ninput NewPetInput {\n    name: String!\n    tag: String\n}\n\ntype Pet {\n    id: Int!\n    name: String!\n    tag: String\n}",
    "type_field_configurations": null,
    "playground": {
        "enabled": false,
        "path": ""
    },
    "engine": {
        "field_configs": [
            {
                "type_name": "Mutation",
                "field_name": "addPet",
                "disable_default_mapping": true,
                "path": [
                    "addPet"
                ]
            },
            {
                "type_name": "Mutation",
                "field_name": "deletePet",
                "disable_default_mapping": true,
                "path": [
                    "deletePet"
                ]
            },
            {
                "type_name": "Query",
                "field_name": "findPetById",
                "disable_default_mapping": true,
                "path": [
                    "findPetById"
                ]
            },
            {
                "type_name": "Query",
                "field_name": "findPets",
                "disable_default_mapping": true,
                "path": [
                    "findPets"
                ]
            }
        ],
        "data_sources": [
            {
                "kind": "REST",
                "name": "addPet",
                "internal": false,
                "root_fields": [
                    {
                        "type": "Mutation",
                        "fields": [
                            "addPet"
                        ]
                    }
                ],
                "config": {
                    "url": "http://petstore.swagger.io/api/pets",
                    "method": "POST",
                    "headers": {},
                    "query": [],
                    "body": "{{ .arguments.newPetInput }}"
                }
            },
            {
                "kind": "REST",
                "name": "deletePet",
                "internal": false,
                "root_fields": [
                    {
                        "type": "Mutation",
                        "fields": [
                            "deletePet"
                        ]
                    }
                ],
                "config": {
                    "url": "http://petstore.swagger.io/api/pets/{{.arguments.id}}",
                    "method": "DELETE",
                    "headers": {},
                    "query": [],
                    "body": ""
                }
            },
            {
                "kind": "REST",
                "name": "findPetById",
                "internal": false,
                "root_fields": [
                    {
                        "type": "Query",
                        "fields": [
                            "findPetById"
                        ]
                    }
                ],
                "config": {
                    "url": "http://petstore.swagger.io/api/pets/{{.arguments.id}}",
                    "method": "GET",
                    "headers": {},
                    "query": [],
                    "body": ""
                }
            },
            {
                "kind": "REST",
                "name": "findPets",
                "internal": false,
                "root_fields": [
                    {
                        "type": "Query",
                        "fields": [
                            "findPets"
                        ]
                    }
                ],
                "config": {
                    "url": "http://petstore.swagger.io/api/pets?limit={{.arguments.limit}}\u0026tags={{.arguments.tags}}",
                    "method": "GET",
                    "headers": {},
                    "query": [],
                    "body": ""
                }
            }
        ],
        "global_headers": null
    },
    "proxy": {
        "features": {
            "use_immutable_headers": false
        },
        "auth_headers": {},
        "sse_use_post": false,
        "request_headers": null,
        "use_response_extensions": {
            "on_error_forwarding": false
        },
        "request_headers_rewrite": null
    },
    "subgraph": {
        "sdl": ""
    },
    "supergraph": {
        "subgraphs": null,
        "merged_sdl": "",
        "global_headers": null,
        "disable_query_batching": false
    },
    "introspection": {
        "disabled": false
    }
}`

func TestGraphQLConfigAdapter_OpenAPI(t *testing.T) {
	adapter := NewOpenAPIAdapter("my-org-id", []byte(petstoreExpandedOpenAPI3))

	actualApiDefinition, err := adapter.Import()
	require.NoError(t, err)

	require.Equal(t, "Swagger Petstore", actualApiDefinition.Name)
	require.True(t, actualApiDefinition.GraphQL.Enabled)
	require.True(t, actualApiDefinition.Active)
	require.Equal(t, apidef.GraphQLExecutionModeExecutionEngine, actualApiDefinition.GraphQL.ExecutionMode)
	require.Equal(t, expectedOpenAPIGraphQLSchema, actualApiDefinition.GraphQL.Schema)

	data, err := json.Marshal(actualApiDefinition)
	require.NoError(t, err)

	actualGraphqlConfig, _, _, err := jsonparser.Get(data, "graphql")
	require.NoError(t, err)

	dst := bytes.NewBuffer(nil)
	err = json.Indent(dst, actualGraphqlConfig, "", "    ")
	require.NoError(t, err)
	require.Equal(t, expectedOpenAPIGraphQLConfig, dst.String())
}
